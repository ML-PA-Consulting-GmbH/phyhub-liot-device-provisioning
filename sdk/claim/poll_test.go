package claim

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fastPoller returns a Poller configured for snappy test runs: every
// timer is shrunk so a multi-iteration test completes in milliseconds
// rather than minutes. Narrative output is captured into the returned
// buffer; tests that need to assert on output read it directly.
func fastPoller() (*Poller, *bytes.Buffer) {
	var buf bytes.Buffer
	return &Poller{
		Interval:         5 * time.Millisecond,
		FastPollInterval: 5 * time.Millisecond,
		FastPollDuration: 50 * time.Millisecond,
		EscalateAfter:    50 * time.Millisecond,
		Output:           &buf,
	}, &buf
}

// mockAppstore returns an httptest.Server whose claim-status endpoint
// returns the responses produced by `responder`. responder is called once
// per request with the request count (0-indexed) so a test can simulate
// state transitions.
func mockAppstore(t *testing.T, responder func(call int, w http.ResponseWriter, r *http.Request)) (*httptest.Server, func() int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1) - 1

		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type: got %q, want application/json", ct)
		}
		// The body must always carry the token: the Appstore uses it as
		// the lookup key, and a missing token would silently turn every
		// poll into a 4xx.
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		if req.Token == "" {
			t.Errorf("request missing token field")
		}

		responder(int(n), w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() int32 { return atomic.LoadInt32(&calls) }
}

func writeStatus(t *testing.T, w http.ResponseWriter, status string, ttlSeconds int) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{"status": status}
	if ttlSeconds > 0 {
		body["ttl_seconds"] = ttlSeconds
	}
	_ = json.NewEncoder(w).Encode(body)
}

func TestPollUntilClaimed_ImmediateClaimed(t *testing.T) {
	srv, _ := mockAppstore(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeStatus(t, w, "claimed", 7200)
	})

	p, _ := fastPoller()
	claimed, ttl, err := p.Run(context.Background(), srv.URL, "ABCD-1234-EFGH")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !claimed {
		t.Error("expected claimed=true")
	}
	if ttl != 2*time.Hour {
		t.Errorf("ttl: got %v, want 2h", ttl)
	}
}

func TestPollUntilClaimed_ClaimedWithoutTTLUsesDefault(t *testing.T) {
	srv, _ := mockAppstore(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeStatus(t, w, "claimed", 0) // omit ttl_seconds
	})

	p, _ := fastPoller()
	_, ttl, err := p.Run(context.Background(), srv.URL, "TOK")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ttl != DefaultClaimTTL {
		t.Errorf("ttl: got %v, want DefaultClaimTTL=%v", ttl, DefaultClaimTTL)
	}
}

func TestPollUntilClaimed_PendingThenClaimed(t *testing.T) {
	srv, calls := mockAppstore(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if call < 2 {
			writeStatus(t, w, "pending", 0)
		} else {
			writeStatus(t, w, "claimed", 3600)
		}
	})

	p, _ := fastPoller()
	claimed, ttl, err := p.Run(context.Background(), srv.URL, "TOK")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !claimed {
		t.Error("expected claimed=true")
	}
	if ttl != time.Hour {
		t.Errorf("ttl: got %v, want 1h", ttl)
	}
	if calls() < 3 {
		t.Errorf("expected at least 3 calls (2 pending + 1 claimed), got %d", calls())
	}
}

func TestPollUntilClaimed_Expired(t *testing.T) {
	srv, _ := mockAppstore(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if call == 0 {
			writeStatus(t, w, "pending", 0)
		} else {
			writeStatus(t, w, "expired", 0)
		}
	})

	p, buf := fastPoller()
	claimed, ttl, err := p.Run(context.Background(), srv.URL, "TOK")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claimed {
		t.Error("expected claimed=false on expired")
	}
	if ttl != 0 {
		t.Errorf("ttl on expired: got %v, want 0", ttl)
	}
	if !strings.Contains(buf.String(), "expired") {
		t.Errorf("expected operator-visible 'expired' message, output was:\n%s", buf.String())
	}
}

func TestPollUntilClaimed_TransientErrorsRecover(t *testing.T) {
	// Network/server hiccups must not abort the loop; the tool keeps
	// trying until the token is claimed or expires. This is the property
	// that lets a device boot before the Appstore is reachable.
	srv, _ := mockAppstore(t, func(call int, w http.ResponseWriter, r *http.Request) {
		switch {
		case call < 2:
			http.Error(w, "internal", http.StatusInternalServerError)
		case call < 4:
			writeStatus(t, w, "pending", 0)
		default:
			writeStatus(t, w, "claimed", 3600)
		}
	})

	p, _ := fastPoller()
	claimed, _, err := p.Run(context.Background(), srv.URL, "TOK")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !claimed {
		t.Error("expected claimed=true after transient errors")
	}
}

func TestPollUntilClaimed_UnexpectedStatusKeepsTrying(t *testing.T) {
	srv, _ := mockAppstore(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if call == 0 {
			writeStatus(t, w, "wat", 0) // not in {claimed, expired, pending}
		} else {
			writeStatus(t, w, "claimed", 3600)
		}
	})

	p, _ := fastPoller()
	claimed, _, err := p.Run(context.Background(), srv.URL, "TOK")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !claimed {
		t.Error("expected claimed=true after unexpected status")
	}
}

func TestPollUntilClaimed_ContextCancelStops(t *testing.T) {
	srv, _ := mockAppstore(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeStatus(t, w, "pending", 0)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		claimed bool
		err     error
	}, 1)
	p, _ := fastPoller()
	go func() {
		c, _, err := p.Run(ctx, srv.URL, "TOK")
		done <- struct {
			claimed bool
			err     error
		}{c, err}
	}()

	// Let it spin once or twice, then cancel.
	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case got := <-done:
		if got.err == nil {
			t.Error("expected ctx error, got nil")
		}
		if got.claimed {
			t.Error("claimed must be false on cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PollUntilClaimed did not return after ctx cancel")
	}
}

func TestPollUntilClaimed_DefaultIntervalAppliedWhenZero(t *testing.T) {
	// `interval <= 0` is documented to fall back to DefaultPollInterval.
	// We can't wait 60s in a test, so we just verify the function still
	// returns when the first poll happens to succeed (which it does
	// immediately, before any sleep).
	srv, _ := mockAppstore(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeStatus(t, w, "claimed", 3600)
	})

	done := make(chan error, 1)
	go func() {
		_, _, err := PollUntilClaimed(context.Background(), srv.URL, "TOK", 0)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first poll should fire immediately even with interval=0")
	}
}

// --- claimEscalationLine -------------------------------------------------

func TestClaimEscalationLine_ErrIncludesDetail(t *testing.T) {
	got := claimEscalationLine("err", "dial: connection refused", 7*time.Minute)
	if !strings.Contains(got, "7m0s") {
		t.Errorf("missing duration in: %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("missing underlying error: %q", got)
	}
}

func TestClaimEscalationLine_PendingHasNoDetail(t *testing.T) {
	got := claimEscalationLine("pending", "", 6*time.Minute)
	if !strings.Contains(got, "6m0s") {
		t.Errorf("missing duration: %q", got)
	}
	if strings.Contains(got, "error") {
		t.Errorf("pending escalation should not mention errors: %q", got)
	}
}

func TestClaimEscalationLine_UnknownStateWithDetail(t *testing.T) {
	got := claimEscalationLine("unexpected:foo", `status="foo"`, 5*time.Minute)
	if !strings.Contains(got, "unexpected:foo") || !strings.Contains(got, `status="foo"`) {
		t.Errorf("unexpected-state escalation lost detail: %q", got)
	}
}

func TestClaimEscalationLine_UnknownStateNoDetailIsEmpty(t *testing.T) {
	if got := claimEscalationLine("weird", "", time.Minute); got != "" {
		t.Errorf("expected empty line when no detail, got %q", got)
	}
}

// --- truncate -----------------------------------------------------------

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"short", 200, "short"},
		{"abcdefghij", 5, "abcde..."},
		{"", 10, ""},
	}
	for _, c := range cases {
		if got := truncate(c.in, c.max); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}
