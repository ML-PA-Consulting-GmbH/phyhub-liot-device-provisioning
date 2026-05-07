package claim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// DefaultPollInterval is the steady-state cadence for PollUntilClaimed
// (used after DefaultFastPollDuration has elapsed). The caller can
// override this via Poller.Interval or the `interval` argument.
const DefaultPollInterval = 60 * time.Second

// DefaultFastPollInterval is the cadence used for the first
// DefaultFastPollDuration after the token is displayed. Quick feedback
// during the window when the operator is actively typing the token.
const DefaultFastPollInterval = 20 * time.Second

// DefaultFastPollDuration is how long the fast-poll phase lasts before
// falling back to the slower steady-state interval.
const DefaultFastPollDuration = 3 * time.Minute

// DefaultEscalateAfter is how long the claim-status loop may stay in
// the same state before printing an extra diagnostic line. Reset on
// state change.
const DefaultEscalateAfter = 5 * time.Minute

// DefaultClaimHTTPTimeout is the per-request timeout for claim-status
// calls. The explicit timeout protects against a stuck Appstore: a hung
// body read would otherwise block forever even with a request-context
// deadline. Override by injecting a custom Poller.HTTPClient.
const DefaultClaimHTTPTimeout = 30 * time.Second

// Poller bundles the tunables for PollUntilClaimed. Zero-valued fields
// fall back to the package defaults; tests inject smaller values to
// keep the loop snappy.
//
// Output is the destination for narrative log lines ("Waiting for you to
// enter the token...", "Token expired...", etc.). When nil, output is
// discarded; callers that want operator-visible progress should set it
// to os.Stdout (or a captured buffer in tests).
type Poller struct {
	Interval         time.Duration
	FastPollInterval time.Duration
	FastPollDuration time.Duration
	EscalateAfter    time.Duration
	HTTPClient       *http.Client
	Output           io.Writer
}

func (p *Poller) interval() time.Duration {
	if p.Interval > 0 {
		return p.Interval
	}
	return DefaultPollInterval
}

func (p *Poller) fastInterval() time.Duration {
	if p.FastPollInterval > 0 {
		return p.FastPollInterval
	}
	return DefaultFastPollInterval
}

func (p *Poller) fastDuration() time.Duration {
	if p.FastPollDuration > 0 {
		return p.FastPollDuration
	}
	return DefaultFastPollDuration
}

func (p *Poller) escalateAfter() time.Duration {
	if p.EscalateAfter > 0 {
		return p.EscalateAfter
	}
	return DefaultEscalateAfter
}

func (p *Poller) httpClient() *http.Client {
	if p.HTTPClient == nil {
		p.HTTPClient = &http.Client{Timeout: DefaultClaimHTTPTimeout}
	}
	return p.HTTPClient
}

func (p *Poller) output() io.Writer {
	if p.Output == nil {
		return io.Discard
	}
	return p.Output
}

// claim status values returned by the Appstore.
const (
	statusClaimed = "claimed"
	statusExpired = "expired"
	statusPending = "pending"
)

// PollUntilClaimed polls the Appstore claim-status endpoint until the token
// is claimed or expires.
//
// Output is deduplicated and escalates after EscalateAfter (5min default)
// in the same state; see EscalateAfter.
//
// Returns:
//   - (true, ttl, nil): token claimed; caller has `ttl` to complete the
//     registration before the backend marks it expired
//   - (false, 0, nil): token expired before the user entered it; caller
//     should regenerate and retry the loop
//   - (false, 0, err): context cancelled or terminal error
//
// statusURL is typically <store-url>/device/v3/provisioning/claim/status.
// interval defaults to DefaultPollInterval when zero.
func PollUntilClaimed(ctx context.Context, statusURL, token string, interval time.Duration) (bool, time.Duration, error) {
	return (&Poller{Interval: interval}).Run(ctx, statusURL, token)
}

// Run is PollUntilClaimed using the Poller's tunables. See
// PollUntilClaimed for return-value semantics.
func (p *Poller) Run(ctx context.Context, statusURL, token string) (bool, time.Duration, error) {
	interval := p.interval()
	fastInterval := p.fastInterval()
	fastDuration := p.fastDuration()
	escalateAfter := p.escalateAfter()
	httpClient := p.httpClient()

	p.logf("Waiting for you to enter the token in the L-IoT Appstore...")

	// stateKey labels the current observable state ("pending", "err"); we
	// only print a new line when stateKey changes. stateSince is when we
	// entered it; stateDetail is the latest underlying detail (raw error
	// message, raw status string) used by the escalation print.
	var (
		stateKey    string
		stateSince  time.Time
		stateDetail string
		escalated   bool
	)
	transition := func(key, msg, detail string) {
		stateDetail = detail
		if key == stateKey {
			return
		}
		stateKey = key
		stateSince = time.Now()
		escalated = false
		p.logf("%s", msg)
	}

	startTime := time.Now()
	for tick := 0; ; tick++ {
		// Skip the sleep on the very first iteration so we query
		// immediately, since it might already be claimed (e.g. the device
		// rebooted after the user entered the token, on the previous
		// boot we hadn't gotten as far as observing the claim).
		//
		// Subsequent iterations use FastPollInterval for the first
		// FastPollDuration (quick feedback while the user is actively
		// entering the token), then fall back to the caller-supplied
		// steady-state interval.
		if tick > 0 {
			pollInterval := interval
			if time.Since(startTime) < fastDuration {
				pollInterval = fastInterval
			}
			select {
			case <-ctx.Done():
				return false, 0, ctx.Err()
			case <-time.After(pollInterval):
			}
		}

		// Escalate if we've been stuck in the same non-terminal state.
		if !escalated && stateKey != "" && time.Since(stateSince) >= escalateAfter {
			if line := claimEscalationLine(stateKey, stateDetail, time.Since(stateSince)); line != "" {
				p.logf("%s", line)
			}
			escalated = true
		}

		status, ttlSeconds, err := queryClaimStatus(ctx, httpClient, statusURL, token)
		if err != nil {
			transition("err",
				"Cannot reach the L-IoT Appstore yet; will keep trying",
				err.Error())
			continue
		}

		switch status {
		case statusClaimed:
			transition("claimed", "Token accepted, registering device...", "")
			ttl := time.Duration(ttlSeconds) * time.Second
			if ttl <= 0 {
				// Backend didn't supply ttl_seconds; fall back
				// to the spec default so the caller still has a
				// deadline to enforce.
				ttl = DefaultClaimTTL
			}
			return true, ttl, nil
		case statusExpired:
			transition("expired", "Token expired before registration completed", "")
			return false, 0, nil
		case statusPending:
			// Suppressed by design: the opening "Waiting for you to
			// enter the token..." line already conveys this. We do
			// still update stateKey so escalation tracks it.
			if stateKey != "pending" {
				stateKey = "pending"
				stateSince = time.Now()
				escalated = false
				stateDetail = ""
			}
		default:
			transition("unexpected:"+status,
				"Unexpected response from the Appstore; will keep trying",
				fmt.Sprintf("status=%q", status))
		}
	}
}

// DefaultClaimTTL is used when the Appstore reports "claimed" without
// supplying ttl_seconds. Matches the claiming-token provisioning spec's working
// value of 3 hours.
const DefaultClaimTTL = 3 * time.Hour

func claimEscalationLine(key, detail string, age time.Duration) string {
	d := age.Round(time.Second)
	switch key {
	case "err":
		return fmt.Sprintf("Still cannot reach the L-IoT Appstore after %s; last error: %s", d, truncate(detail, 200))
	case "pending":
		return fmt.Sprintf("Still waiting for the token to be entered in the Appstore (it has been %s)", d)
	}
	if detail != "" {
		return fmt.Sprintf("State %q persisted for %s (%s)", key, d, detail)
	}
	return ""
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// logf writes a single timestamped line to the Poller's Output. The
// `[HH:MM:SS]` stamp matches the observer's output style.
func (p *Poller) logf(format string, args ...any) {
	fmt.Fprintf(p.output(), "[%s] "+format+"\n",
		append([]any{time.Now().Format("15:04:05")}, args...)...)
}

type claimStatusRequest struct {
	Token string `json:"token"`
}

type claimStatusResponse struct {
	Status     string `json:"status"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

func queryClaimStatus(ctx context.Context, client *http.Client, statusURL, token string) (string, int, error) {
	body, err := json.Marshal(claimStatusRequest{Token: token})
	if err != nil {
		return "", 0, fmt.Errorf("marshal claim-status request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, statusURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		// Distinguish a client-side timeout from other transport
		// errors so the operator can tell "Appstore is slow" from
		// "Appstore is unreachable". Parent-context cancellation is
		// surfaced verbatim so callers can detect it via errors.Is.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", 0, ctxErr
		}
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Timeout() {
			return "", 0, fmt.Errorf("claim-status request timed out after %s: %w", client.Timeout, err)
		}
		return "", 0, err
	}
	defer resp.Body.Close()

	var result claimStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", 0, fmt.Errorf("decode response: %w", err)
	}

	return result.Status, result.TTLSeconds, nil
}
