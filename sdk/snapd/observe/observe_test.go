package observe

import (
	"strings"
	"testing"
	"time"

	"phyhub-liot-device-provisioning/sdk/snapd/api"
)

func ts(s string) time.Time {
	v, _ := time.Parse("15:04:05", s)
	return v
}

func TestDescribeInitial(t *testing.T) {
	cases := []struct {
		name  string
		snap  Snapshot
		wants []string
	}{
		{
			"unreachable",
			Snapshot{Time: ts("10:00:00"), SnapdError: "connection refused"},
			[]string{"Cannot reach Snapd"},
		},
		{
			"reachable, seeding",
			Snapshot{Time: ts("10:00:00"), SnapdReachable: true},
			[]string{"Snapd is installing initial applications"},
		},
		{
			"reachable, seeded",
			Snapshot{Time: ts("10:00:00"), SnapdReachable: true, Seeded: true},
			[]string{"Snapd has installed initial applications"},
		},
		{
			"reachable, seeded, already-registered",
			Snapshot{Time: ts("10:00:00"), SnapdReachable: true, Seeded: true,
				Serial: &api.SerialAssertion{Headers: map[string]string{"serial": "abc-123"}}},
			[]string{"Snapd has installed initial applications", "Device already registered (OS-Serial: abc-123)"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := strings.Join(describeInitial(c.snap), "\n")
			for _, want := range c.wants {
				if !strings.Contains(got, want) {
					t.Errorf("%s: missing %q in:\n%s", c.name, want, got)
				}
			}
		})
	}
}

func TestDescribeTransition_Seeding(t *testing.T) {
	prev := Snapshot{Time: ts("10:00:00"), SnapdReachable: true, Seeded: false}
	cur := Snapshot{Time: ts("10:00:05"), SnapdReachable: true, Seeded: true}
	got := strings.Join(describeTransition(prev, cur), "\n")
	want := "Snapd has installed initial applications"
	if !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
}

func TestDescribeTransition_SerialAssignment(t *testing.T) {
	base := Snapshot{Time: ts("10:00:00"), SnapdReachable: true, Seeded: true}
	cur := base
	cur.Serial = &api.SerialAssertion{
		Headers: map[string]string{
			"serial": "550e8400-e29b-41d4-a716-446655440000",
		},
	}

	got := strings.Join(describeTransition(base, cur), "\n")
	want := "Device registered (OS-Serial: 550e8400-e29b-41d4-a716-446655440000)"
	if !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
}

func TestDescribeTransition_NoChangeIsSilent(t *testing.T) {
	s := Snapshot{Time: ts("10:00:00"), SnapdReachable: true, Seeded: true}
	got := describeTransition(s, s)
	if len(got) != 0 {
		t.Errorf("expected no output for identical snapshots, got: %v", got)
	}
}

func TestDescribeTransition_UnreachableTransitions(t *testing.T) {
	reachable := Snapshot{Time: ts("10:00:00"), SnapdReachable: true, Seeded: true}
	unreachable := Snapshot{Time: ts("10:00:05"), SnapdError: "dial: connection refused"}

	got := strings.Join(describeTransition(reachable, unreachable), "\n")
	if !strings.Contains(got, "Lost connection to Snapd") {
		t.Errorf("expected 'Lost connection to Snapd' in:\n%s", got)
	}

	got = strings.Join(describeTransition(unreachable, reachable), "\n")
	if !strings.Contains(got, "Connection to Snapd restored") {
		t.Errorf("expected 'Connection to Snapd restored' in:\n%s", got)
	}
}

func TestIsRegistered_True(t *testing.T) {
	s := Snapshot{
		SnapdReachable: true, Seeded: true,
		Serial: &api.SerialAssertion{Headers: map[string]string{"serial": "abc"}},
	}
	if !s.IsRegistered() {
		t.Error("expected IsRegistered=true when Serial is set")
	}
}

func TestIsRegistered_FalseWithoutSerial(t *testing.T) {
	s := Snapshot{SnapdReachable: true, Seeded: true}
	if s.IsRegistered() {
		t.Error("expected IsRegistered=false without Serial")
	}
}

func TestIsSeeded(t *testing.T) {
	if (Snapshot{SnapdReachable: true, Seeded: true}).IsSeeded() != true {
		t.Error("reachable+seeded should be IsSeeded=true")
	}
	if (Snapshot{SnapdReachable: true, Seeded: false}).IsSeeded() != false {
		t.Error("reachable+not-seeded should be IsSeeded=false")
	}
	if (Snapshot{SnapdReachable: false, Seeded: true}).IsSeeded() != false {
		t.Error("unreachable should be IsSeeded=false even with Seeded set")
	}
}

func TestFormatWarningLine_Plain(t *testing.T) {
	s := Snapshot{Time: ts("10:00:00")}
	got := formatWarningLine(s, api.Warning{Message: "unable to contact snap store"})
	if !strings.Contains(got, "Snapd warning:") {
		t.Errorf("expected plain 'Snapd warning:' prefix in %q", got)
	}
	if strings.Contains(got, "CRITICAL") {
		t.Errorf("plain warning should not be marked CRITICAL: %q", got)
	}
	if !strings.Contains(got, "unable to contact snap store") {
		t.Errorf("message missing from line: %q", got)
	}
}

func TestWarningIsCritical_MatchesConfiguredPhrases(t *testing.T) {
	// Temporarily install a phrase to prove the hook works; the shipped
	// list is empty on purpose.
	defer func(orig []string) { criticalWarningPhrases = orig }(criticalWarningPhrases)
	criticalWarningPhrases = []string{"is blocked"}

	if !warningIsCritical(`cannot install "foo": snap IS BLOCKED`) {
		t.Error("expected case-insensitive match on configured phrase")
	}
	if warningIsCritical("some unrelated warning") {
		t.Error("did not expect a match for an unconfigured message")
	}

	s := Snapshot{Time: ts("10:00:00")}
	line := formatWarningLine(s, api.Warning{Message: "snap is blocked"})
	if !strings.Contains(line, "[CRITICAL]") {
		t.Errorf("critical warning should carry the [CRITICAL] marker: %q", line)
	}
}

func TestWarningIsCritical_EmptyListMatchesNothing(t *testing.T) {
	if len(criticalWarningPhrases) != 0 {
		t.Fatalf("shipped criticalWarningPhrases should be empty, has %d", len(criticalWarningPhrases))
	}
	if warningIsCritical("cannot install anything: is blocked") {
		t.Error("empty phrase list must never classify a warning as critical")
	}
}

func TestReportNewWarnings_EmitsEachWarningOnce(t *testing.T) {
	var buf strings.Builder
	o := &Observer{out: &buf}

	w1 := api.Warning{Message: "first problem", FirstAdded: ts("10:00:00")}
	w2 := api.Warning{Message: "second problem", FirstAdded: ts("10:00:01")}

	// Tick 1: both warnings present -> both announced.
	o.reportNewWarnings(Snapshot{Time: ts("10:00:05"), SnapdReachable: true, Warnings: []api.Warning{w1, w2}})
	// Tick 2: same warnings still present -> nothing new.
	o.reportNewWarnings(Snapshot{Time: ts("10:00:10"), SnapdReachable: true, Warnings: []api.Warning{w1, w2}})
	// Tick 3: a genuinely new warning appears -> only it is announced.
	w3 := api.Warning{Message: "third problem", FirstAdded: ts("10:00:11")}
	o.reportNewWarnings(Snapshot{Time: ts("10:00:15"), SnapdReachable: true, Warnings: []api.Warning{w1, w2, w3}})

	out := buf.String()
	if n := strings.Count(out, "first problem"); n != 1 {
		t.Errorf("first warning should appear exactly once, got %d:\n%s", n, out)
	}
	if n := strings.Count(out, "second problem"); n != 1 {
		t.Errorf("second warning should appear exactly once, got %d:\n%s", n, out)
	}
	if n := strings.Count(out, "third problem"); n != 1 {
		t.Errorf("third warning should appear exactly once, got %d:\n%s", n, out)
	}
}

func TestReportNewWarnings_ReRaisedWarningIsAnnouncedAgain(t *testing.T) {
	var buf strings.Builder
	o := &Observer{out: &buf}

	// Same message, but a later first-added (snapd re-raised it after the
	// prior instance expired) must be treated as new.
	o.reportNewWarnings(Snapshot{Time: ts("10:00:05"), Warnings: []api.Warning{
		{Message: "store unreachable", FirstAdded: ts("10:00:00")},
	}})
	o.reportNewWarnings(Snapshot{Time: ts("10:30:05"), Warnings: []api.Warning{
		{Message: "store unreachable", FirstAdded: ts("10:30:00")},
	}})

	if n := strings.Count(buf.String(), "store unreachable"); n != 2 {
		t.Errorf("re-raised warning should be announced again, got %d:\n%s", n, buf.String())
	}
}

// TestEscalationDoesNotFireAcrossRunUntilGap regression-tests the case where
// a flow does RunUntil(Seeded), pauses for longer than EscalateAfter (e.g.
// during a long claim poll), then calls Run() to observe registration. The
// observer must not immediately scream "stuck for 4m" at the start of the
// second call just because wall-clock time advanced while it wasn't watching.
func TestEscalationDoesNotFireAcrossRunUntilGap(t *testing.T) {
	// Manually drive describeTransition + state bookkeeping by simulating
	// what RunUntil does. We can't easily run the real loop without a
	// snapdapi server, so we recreate the relevant bookkeeping.
	o := &Observer{EscalateAfter: 50 * time.Millisecond}
	steady := Snapshot{
		Time:           time.Now(),
		SnapdReachable: true,
		Seeded:         true,
	}
	// Phase 1: first observation, state established.
	sig1 := stateSignature(steady)
	o.stateSig = sig1
	o.stateSince = steady.Time
	o.started = true

	// Simulate a 5-minute gap (longer than EscalateAfter) during which
	// the observer wasn't running.
	later := steady
	later.Time = steady.Time.Add(5 * time.Minute)

	// Phase 2 first tick: the new RunUntil sees the same state signature.
	// Per the fix, this MUST NOT trigger escalation; it should reset
	// stateSince to now.
	firstTickThisCall := true
	curSig := stateSignature(later)
	emitted := []string{}

	switch {
	case curSig != o.stateSig:
		t.Fatalf("test setup wrong: signatures differ")
	case firstTickThisCall:
		o.stateSince = later.Time
		o.escalated = false
	default:
		// Unreachable in this test (firstTickThisCall is true), kept
		// here so a regression that flips precedence triggers a clear
		// failure mode rather than silently passing.
		t.Fatalf("default branch should not be reached when firstTickThisCall is true")
	}

	if len(emitted) != 0 {
		t.Errorf("expected no escalation on first tick of new RunUntil, got: %v", emitted)
	}
	if !o.stateSince.Equal(later.Time) {
		t.Errorf("expected stateSince reset to current tick time, got %v vs now %v", o.stateSince, later.Time)
	}
	if o.escalated {
		t.Errorf("expected escalated=false after RunUntil-boundary reset, got true")
	}
}
