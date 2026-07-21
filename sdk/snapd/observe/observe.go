// Package observe polls snapd's view of the device-registration flow and
// emits a narrative event log: one line each time the observable state
// actually changes, in plain language ("snapd is seeded", "device
// registered, OS-Serial: ..."). The console stays quiet between
// transitions.
//
// The Observer is reusable: a flow can call RunUntil(ctx, Seeded) first, do
// some local work, then call RunUntil(ctx, Registered). The second call
// resumes from the first call's last observation and emits only further
// transitions; no recap of state already announced.
//
// What we observe:
//   - /v2/system-info → snapd reachable + seeded
//   - /v2/model/serial → serial assertion (the universal "registered" signal)
//
// We do NOT poll the L-IoT registration-data endpoint: registration progress
// and failures are visible via `snap changes` and `journalctl -u snapd`,
// which give richer detail than this narrative ever could.
package observe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"phyhub-liot-device-provisioning/sdk/snapd/api"
)

// DefaultInterval is the polling cadence when zero is passed to New.
const DefaultInterval = 5 * time.Second

// DefaultEscalateAfter is how long a single observable state may persist
// before the observer prints an additional diagnostic line containing the
// raw underlying error (or other detail). The escalation prints once per
// stuck-state and resets when the state changes. Used as the default for
// Observer.EscalateAfter; tests may override it on the Observer instance.
const DefaultEscalateAfter = 2 * time.Minute

// Snapshot is the union of all observable signals at a single point in
// time. A zero-value Snapshot represents "nothing observed yet".
type Snapshot struct {
	Time time.Time

	// SnapdReachable is true when the last GET against snapd succeeded.
	SnapdReachable bool
	// SnapdError carries the most recent reachability error, if any.
	SnapdError string

	// Seeded reflects /v2/system-info "seeded". Meaningful only when
	// SnapdReachable is true.
	Seeded bool

	// Serial is the device's current serial assertion, or nil when no
	// serial has been issued yet. Its presence is the authoritative
	// "device is registered" signal.
	Serial *api.SerialAssertion

	// Warnings holds snapd's currently-active warnings at this tick.
	// Meaningful only when SnapdReachable is true; empty otherwise.
	Warnings []api.Warning
}

// IsRegistered reports whether the snapshot shows a serial assertion,
// which is the authoritative terminal signal.
func (s Snapshot) IsRegistered() bool { return s.Serial != nil }

// IsSeeded reports whether snapd is reachable AND has finished seeding.
func (s Snapshot) IsSeeded() bool { return s.SnapdReachable && s.Seeded }

// StopCondition decides whether RunUntil should stop. The condition is
// evaluated against every fresh snapshot.
type StopCondition func(Snapshot) bool

// Registered stops once the device has a serial assertion.
func Registered(s Snapshot) bool { return s.IsRegistered() }

// Seeded stops once snapd is reachable and has finished seeding.
func Seeded(s Snapshot) bool { return s.IsSeeded() }

// Observer drives the polling loop and emits narrative event lines on
// transitions. Reusable across multiple RunUntil calls; state from earlier
// observations is retained so the next call only emits new transitions.
//
// Not thread-safe: a single Observer must not be used concurrently from
// multiple goroutines. Each provisioning flow constructs its own.
type Observer struct {
	api      *api.Client
	interval time.Duration
	out      io.Writer

	// EscalateAfter overrides DefaultEscalateAfter for this Observer.
	// Set by New; tests may set it directly to shrink the threshold.
	EscalateAfter time.Duration

	started bool
	last    Snapshot

	// stateSig is the signature of the most recently observed state;
	// stateSince is when we entered it. Used by the escalation logic to
	// decide when to print extra diagnostic detail for a stuck state.
	stateSig   string
	stateSince time.Time
	escalated  bool

	// seenWarnings dedupes proactively-emitted warnings across ticks (and
	// across successive RunUntil calls on the same Observer) so a standing
	// warning is announced once, not on every poll. Keyed by warningKey.
	seenWarnings map[string]bool
}

// Option configures an Observer. Options are applied left-to-right;
// later options override earlier ones.
type Option func(*Observer)

// WithInterval sets the polling cadence. Values <= 0 are ignored
// (DefaultInterval is used).
func WithInterval(d time.Duration) Option {
	return func(o *Observer) {
		if d > 0 {
			o.interval = d
		}
	}
}

// WithOutput sets the destination for narrative event lines. When unset,
// output is discarded so a library caller is never spammed by stderr it
// didn't ask for. The provisioning tool passes os.Stdout.
func WithOutput(w io.Writer) Option {
	return func(o *Observer) { o.out = w }
}

// WithEscalateAfter overrides DefaultEscalateAfter for this Observer.
// See DefaultEscalateAfter for the rationale.
func WithEscalateAfter(d time.Duration) Option {
	return func(o *Observer) {
		if d > 0 {
			o.EscalateAfter = d
		}
	}
}

// New constructs an Observer with safe defaults: DefaultInterval polling
// cadence, output discarded, DefaultEscalateAfter escalation threshold.
// Use options to override.
func New(client *api.Client, opts ...Option) *Observer {
	o := &Observer{
		api:           client,
		interval:      DefaultInterval,
		out:           io.Discard,
		EscalateAfter: DefaultEscalateAfter,
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Run polls until the device has a serial assertion or ctx is cancelled.
// Convenience wrapper around RunUntil(ctx, Registered).
//
// Note: there is no "registration failed" terminal signal in this layer;
// snapd's task system retries on most failures, and genuinely terminal
// failures are visible via `snap changes` and `journalctl -u snapd`. If
// the user gives up they Ctrl+C, which surfaces as ctx.Err().
func (o *Observer) Run(ctx context.Context) (Snapshot, error) {
	return o.RunUntil(ctx, Registered)
}

// RunUntil polls until the stop condition is satisfied or ctx is cancelled.
// On the first tick of an Observer it emits a baseline describing the
// current state; subsequent ticks emit one line per state transition only.
// Reusing the same Observer across calls preserves that baseline so the
// second call continues the narrative rather than restarting it.
//
// If a single state persists longer than EscalateAfter, the observer
// prints one additional diagnostic line so an operator can see the
// underlying error without having to reach for the journal.
func (o *Observer) RunUntil(ctx context.Context, stop StopCondition) (Snapshot, error) {
	// Track whether this is the first tick of *this* RunUntil call (as
	// opposed to the first tick ever). When a flow does
	// RunUntil(Seeded) then later RunUntil(Registered), the gap between
	// the two calls is "untracked" wall-clock time; we don't know what
	// happened during it. If the state hasn't changed across that gap,
	// we treat the current RunUntil as a fresh observation of that
	// state: reset the escalation clock instead of declaring it stuck.
	firstTickThisCall := true

	tick := func() Snapshot {
		cur := o.snapshot(ctx)
		now := cur.Time
		sig := stateSignature(cur)

		switch {
		case !o.started:
			// Very first tick across the entire Observer lifetime.
			o.emit(describeInitial(cur))
			o.stateSig = sig
			o.stateSince = now
			o.escalated = false
			o.started = true
		case sig != o.stateSig:
			// State actually changed since last tick: emit the
			// transition line and restart escalation tracking.
			o.emit(describeTransition(o.last, cur))
			o.stateSig = sig
			o.stateSince = now
			o.escalated = false
		case firstTickThisCall:
			// Same state as last RunUntil call ended in. Don't
			// emit a transition (nothing changed), but reset the
			// escalation clock; the gap between calls is not
			// "stuck" time from the observer's point of view.
			o.stateSince = now
			o.escalated = false
		default:
			// Genuine same-state continuation within this Run.
			if !o.escalated && now.Sub(o.stateSince) >= o.EscalateAfter {
				if lines := o.describeEscalation(ctx, cur, now.Sub(o.stateSince)); len(lines) > 0 {
					o.emit(lines)
				}
				o.escalated = true
			}
		}

		// Announce any newly-appeared warnings regardless of the state
		// machinery above: they are their own signal, orthogonal to the
		// reachable/seeded/registered progression.
		o.reportNewWarnings(cur)

		firstTickThisCall = false
		o.last = cur
		return cur
	}

	if s := tick(); stop(s) {
		return s, nil
	}

	t := time.NewTicker(o.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return o.last, ctx.Err()
		case <-t.C:
			if s := tick(); stop(s) {
				return s, nil
			}
		}
	}
}

func (o *Observer) emit(lines []string) {
	for _, line := range lines {
		fmt.Fprintln(o.out, line)
	}
}

// snapshot performs all observation calls for one tick. Each call is
// independent: a failure on one signal does not zero out the others.
//
// Reachability is determined by /v2/system-info; the "is snapd seeded?"
// signal lives on a separate endpoint (/v2/debug?aspect=seeding) because
// /v2/system-info doesn't expose the seeded flag.
func (o *Observer) snapshot(ctx context.Context) Snapshot {
	s := Snapshot{Time: time.Now()}

	if _, err := o.api.GetSystemInfo(ctx); err != nil {
		s.SnapdReachable = false
		s.SnapdError = err.Error()
		return s
	}
	s.SnapdReachable = true

	// IsSeeded errors are non-fatal: if the debug endpoint isn't there
	// (extremely unlikely on any modern snapd, but defensive), we treat
	// the system as not-yet-seeded and keep polling.
	if seeded, err := o.api.IsSeeded(ctx); err == nil {
		s.Seeded = seeded
	}

	// Serial assertion is the ground truth for "registered".
	// ErrNoSerial just means "not yet"; treat as Serial=nil.
	if serial, serr := o.api.GetSerialAssertion(ctx); serr == nil {
		s.Serial = serial
	} else if !errors.Is(serr, api.ErrNoSerial) {
		// Unexpected fetch error: keep SnapdReachable true (the
		// system-info call succeeded) but log the issue inline as a
		// warning. This stays out of the "transition" path so it
		// doesn't spam.
		fmt.Fprintln(o.out, stamp(s)+" warning: serial endpoint: "+truncate(serr.Error(), 200))
	}

	// Snap warnings always indicate a problem worth surfacing; collect
	// them so the tick loop can announce newly-appeared ones. Best-effort:
	// a fetch error just means no warnings are reported this tick.
	if warnings, werr := o.api.GetWarnings(ctx); werr == nil {
		s.Warnings = warnings
	}

	return s
}

// describeInitial returns the baseline lines for the very first
// observation. The goal is to anchor the user in the current state without
// drowning them in detail: one line for snapd, one line for registration.
func describeInitial(s Snapshot) []string {
	var out []string
	t := stamp(s)

	if !s.SnapdReachable {
		out = append(out, t+" Cannot reach Snapd yet, retrying...")
		return out
	}

	if s.Seeded {
		out = append(out, t+" Snapd has installed initial applications")
	} else {
		out = append(out, t+" Snapd is installing initial applications...")
	}

	if s.Serial != nil {
		out = append(out, t+" Device already registered (OS-Serial: "+s.Serial.Serial()+")")
	}
	return out
}

// describeTransition compares two snapshots and returns one line per
// observable transition between them. Returns an empty slice when nothing
// has changed.
func describeTransition(prev, cur Snapshot) []string {
	var out []string
	t := stamp(cur)

	// snapd reachability transitions
	switch {
	case prev.SnapdReachable && !cur.SnapdReachable:
		out = append(out, t+" Lost connection to Snapd, retrying...")
	case !prev.SnapdReachable && cur.SnapdReachable:
		out = append(out, t+" Connection to Snapd restored")
	}
	if !cur.SnapdReachable {
		return out
	}

	// seeding transitions
	if !prev.Seeded && cur.Seeded {
		out = append(out, t+" Snapd has installed initial applications")
	} else if prev.Seeded && !cur.Seeded {
		// extremely rare: seeded going back to false would mean
		// snapd reset; surface it as a warning.
		out = append(out, t+" Snapd reports it is no longer ready")
	}

	// Serial-assertion transition: nil → present means snapd has just
	// stored the serial; this is the authoritative "registered" event.
	if prev.Serial == nil && cur.Serial != nil {
		out = append(out, t+" Device registered (OS-Serial: "+cur.Serial.Serial()+")")
	}

	return out
}

// describeEscalation returns extra diagnostic lines for a state that has
// persisted longer than EscalateAfter. Returns nil when the current state
// has nothing useful to add.
//
// For the "stuck on registering" case, this also queries snapd for the
// in-progress change list and the registration change's task log, so the
// operator sees the actual state without having to log in and run
// `snap changes` / `snap tasks` themselves.
func (o *Observer) describeEscalation(ctx context.Context, cur Snapshot, age time.Duration) []string {
	t := stamp(cur)
	d := age.Round(time.Second)

	switch {
	case !cur.SnapdReachable:
		return []string{
			fmt.Sprintf("%s Still cannot reach Snapd after %s; last error: %s",
				t, d, truncate(cur.SnapdError, 200)),
		}
	case !cur.Seeded:
		return []string{
			fmt.Sprintf("%s Snapd is still installing initial applications after %s; this can take a few minutes on first boot",
				t, d),
		}
	case cur.Serial == nil:
		out := []string{
			fmt.Sprintf("%s Still registering after %s, diagnostic follows:", t, d),
		}
		out = append(out, o.fetchRegistrationDiagnostic(ctx)...)
		return out
	}
	return nil
}

// fetchRegistrationDiagnostic queries snapd for in-progress changes and
// returns formatted lines roughly equivalent to `snap changes` plus
// `snap tasks <change>` for the registration change. When there are no
// in-progress changes, it falls back to the most recent failed changes
// (up to recentFailedChangesLimit) so the operator sees what went wrong
// on the previous attempt.
//
// Best-effort: if snapd is unresponsive at this moment, we surface that
// fact and move on.
func (o *Observer) fetchRegistrationDiagnostic(ctx context.Context) []string {
	changes, err := o.api.GetChanges(ctx, "in-progress")
	if err != nil {
		return []string{fmt.Sprintf("  (could not fetch snap changes: %v)", err)}
	}
	if len(changes) == 0 {
		// Registration must have completed; show recent failures.
		failed := o.fetchRecentFailedChanges(ctx, recentFailedChangesLimit)
		if len(failed) == 0 {
			return []string{"  (no in-progress changes and no recent failures; check `journalctl -u snapd`)"}
		}
		return failed
	}

	return o.formatInProgressChanges(ctx, changes)
}

// recentFailedChangesLimit caps how many past-failed changes the
// diagnostic dump includes.
const recentFailedChangesLimit = 3

// formatInProgressChanges renders the in-progress change list and, if a
// "become-operational" change is among them, the full task list for it.
func (o *Observer) formatInProgressChanges(ctx context.Context, changes []api.Change) []string {
	var out []string
	out = append(out, "  Snap changes (in-progress):")
	out = append(out, "    ID  Status   Summary")
	for _, ch := range changes {
		out = append(out, fmt.Sprintf("    %-2s  %-7s  %s", ch.ID, ch.Status, ch.Summary))
	}

	// Pick the registration change and dump its full task list.
	var regChange *api.Change
	for i := range changes {
		if changes[i].Kind == "become-operational" {
			regChange = &changes[i]
			break
		}
	}
	if regChange == nil {
		return out
	}

	full, err := o.api.GetChange(ctx, regChange.ID)
	if err != nil {
		out = append(out, fmt.Sprintf("  (could not fetch tasks for change %s: %v)", regChange.ID, err))
		return out
	}
	if len(full.Tasks) == 0 {
		return out
	}

	out = append(out, fmt.Sprintf("  Tasks for change %s (%s):", full.ID, full.Summary))
	out = append(out, "    Status   Task")
	for _, task := range full.Tasks {
		out = append(out, fmt.Sprintf("    %-7s  %s", task.Status, task.Summary))
		for _, line := range task.Log {
			out = append(out, "        "+truncate(line, 200))
		}
	}
	if full.Err != "" {
		out = append(out, fmt.Sprintf("  Change error: %s", truncate(full.Err, 200)))
	}
	return out
}

// fetchRecentFailedChanges returns the formatted lines for up to `limit`
// most-recent changes whose status is Error or that carry a non-empty
// err field. For each, the failing tasks' log lines are included so the
// operator sees the reason without leaving the console.
func (o *Observer) fetchRecentFailedChanges(ctx context.Context, limit int) []string {
	ready, err := o.api.GetChanges(ctx, "ready")
	if err != nil {
		return []string{fmt.Sprintf("  (could not fetch ready snap changes: %v)", err)}
	}

	var failed []api.Change
	for _, ch := range ready {
		if ch.Status == "Error" || ch.Err != "" {
			failed = append(failed, ch)
		}
	}
	if len(failed) == 0 {
		return nil
	}

	// Most recent first.
	sort.Slice(failed, func(i, j int) bool {
		return failed[i].ReadyTime.After(failed[j].ReadyTime)
	})
	if len(failed) > limit {
		failed = failed[:limit]
	}

	out := []string{fmt.Sprintf("  Last %d failed change(s) (most recent first):", len(failed))}
	for _, ch := range failed {
		out = append(out, fmt.Sprintf("    %s  %-7s  %s", ch.ID, ch.Status, ch.Summary))
		if ch.Err != "" {
			out = append(out, "        error: "+truncate(ch.Err, 200))
		}

		full, ferr := o.api.GetChange(ctx, ch.ID)
		if ferr != nil {
			out = append(out, fmt.Sprintf("        (could not fetch tasks: %v)", ferr))
			continue
		}
		for _, task := range full.Tasks {
			out = append(out, fmt.Sprintf("      %-7s  %s", task.Status, task.Summary))
			// Only dump task logs for the actually-broken tasks;
			// Done tasks in a failed change are usually noise.
			if task.Status == "Error" || task.Status == "Doing" {
				for _, line := range task.Log {
					out = append(out, "          "+truncate(line, 200))
				}
			}
		}
	}
	return out
}

// criticalWarningPhrases lists lowercase substrings that mark a snap warning
// as critical rather than merely noteworthy. snapd does not classify warnings
// by severity, so we pattern-match the message text. The list is intentionally
// empty for now: every warning is surfaced regardless, and this is the single
// place to add phrases (e.g. "cannot install", "is blocked") once we know
// which reliably indicate a wedged, non-recoverable install.
var criticalWarningPhrases = []string{}

// warningIsCritical reports whether a warning message matches any known
// critical phrase. Case-insensitive substring match. Returns false while
// criticalWarningPhrases is empty, so today all warnings render the same.
func warningIsCritical(message string) bool {
	lower := strings.ToLower(message)
	for _, phrase := range criticalWarningPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// warningKey identifies a warning for dedup purposes. snapd keys warnings by
// message (re-raising the same text bumps last-added rather than adding a
// row), but a warning that expires and is later raised again gets a fresh
// first-added; including it means such a re-raise is announced again.
func warningKey(w api.Warning) string {
	return w.Message + "\x00" + w.FirstAdded.Format(time.RFC3339Nano)
}

// formatWarningLine renders a single warning as one console line. Critical
// warnings get a distinct, greppable marker so an operator (and log scraping)
// can pick them out.
func formatWarningLine(s Snapshot, w api.Warning) string {
	label := "Snapd warning"
	if warningIsCritical(w.Message) {
		label = "Snapd warning [CRITICAL]"
	}
	return stamp(s) + " " + label + ": " + truncate(w.Message, 200)
}

// reportNewWarnings emits a line for every warning in the snapshot not seen
// before on this Observer. snap warnings always signal a problem worth the
// operator's attention (a blocked or failed install, store-contact failure,
// assertion trouble), so unlike the stuck-state escalation these are surfaced
// as soon as they appear rather than after a grace period. Dedup keeps a
// standing warning from reprinting on every poll.
func (o *Observer) reportNewWarnings(s Snapshot) {
	for _, w := range s.Warnings {
		key := warningKey(w)
		if o.seenWarnings[key] {
			continue
		}
		if o.seenWarnings == nil {
			o.seenWarnings = map[string]bool{}
		}
		o.seenWarnings[key] = true
		o.emit([]string{formatWarningLine(s, w)})
	}
}

// stateSignature returns a stable string capturing what the observer
// considers a single "state". A change in signature triggers a transition
// line; a stable signature for too long triggers an escalation line.
func stateSignature(s Snapshot) string {
	if !s.SnapdReachable {
		// Treat all unreachable states as the same state, regardless
		// of the exact error string. Otherwise a network blip that
		// produces slightly different errors each tick would keep
		// resetting the escalation clock and we'd never escalate.
		return "unreachable"
	}
	parts := []string{"reachable"}
	if s.Seeded {
		parts = append(parts, "seeded")
	} else {
		parts = append(parts, "seeding")
	}
	if s.Serial != nil {
		parts = append(parts, "serial:"+s.Serial.Serial())
	} else {
		parts = append(parts, "no-serial")
	}
	return strings.Join(parts, "|")
}

func stamp(s Snapshot) string {
	return "[" + s.Time.Format("15:04:05") + "]"
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
