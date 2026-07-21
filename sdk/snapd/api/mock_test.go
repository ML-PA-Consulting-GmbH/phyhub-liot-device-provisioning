package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockSnapd spins up an httptest.Server that mimics snapd's local REST
// API. The handler dispatches on method+path and writes envelope-shaped
// responses ({"type":"sync","status-code":200,"result":...}) just like
// real snapd.
//
// Tests register their per-endpoint handlers via the routes map; any
// unrouted path fails the test (catches typos in path constants and
// unintended requests).
type mockSnapd struct {
	t      *testing.T
	server *httptest.Server
	routes map[string]http.HandlerFunc // key: METHOD path
}

func newMockSnapd(t *testing.T) *mockSnapd {
	t.Helper()
	m := &mockSnapd{t: t, routes: map[string]http.HandlerFunc{}}
	m.server = httptest.NewServer(http.HandlerFunc(m.dispatch))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockSnapd) dispatch(w http.ResponseWriter, r *http.Request) {
	// Match on path only first; query strings (e.g. ?select=in-progress)
	// are checked inside the handler.
	key := r.Method + " " + r.URL.Path
	if h, ok := m.routes[key]; ok {
		h(w, r)
		return
	}
	m.t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
	http.Error(w, "no route", http.StatusNotImplemented)
}

func (m *mockSnapd) handle(method, path string, h http.HandlerFunc) {
	m.routes[method+" "+path] = h
}

func (m *mockSnapd) client() *Client {
	return NewClient(WithHTTPClient(m.server.Client()), WithBaseURL(m.server.URL))
}

// writeSync writes a snapd-style sync response with the given JSON-encodable
// result and HTTP status code.
func writeSync(t *testing.T, w http.ResponseWriter, status int, result any) {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}
	env := map[string]any{
		"type":        "sync",
		"status-code": status,
		"result":      json.RawMessage(raw),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}

// writeError writes a snapd-style error envelope.
func writeError(t *testing.T, w http.ResponseWriter, status int, message string) {
	t.Helper()
	env := map[string]any{
		"type":        "error",
		"status-code": status,
		"result":      map[string]string{"message": message},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}

// ---- GetStoreURL ---------------------------------------------------------

func TestGetStoreURL_Success(t *testing.T) {
	m := newMockSnapd(t)
	m.handle("GET", pathAppstoreURL, func(w http.ResponseWriter, r *http.Request) {
		writeSync(t, w, http.StatusOK, map[string]string{"url": "https://appstore.example.com/"})
	})

	got, err := m.client().GetStoreURL(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "https://appstore.example.com" {
		t.Errorf("trailing slash not stripped: got %q", got)
	}
}

func TestGetStoreURL_EmptyURLIsError(t *testing.T) {
	m := newMockSnapd(t)
	m.handle("GET", pathAppstoreURL, func(w http.ResponseWriter, r *http.Request) {
		writeSync(t, w, http.StatusOK, map[string]string{"url": ""})
	})

	_, err := m.client().GetStoreURL(context.Background())
	if err == nil || !strings.Contains(err.Error(), "empty Appstore URL") {
		t.Errorf("expected empty-URL error, got %v", err)
	}
}

func TestGetStoreURL_SnapdErrorIsSurfaced(t *testing.T) {
	m := newMockSnapd(t)
	m.handle("GET", pathAppstoreURL, func(w http.ResponseWriter, r *http.Request) {
		writeError(t, w, http.StatusInternalServerError, "no device-service URL configured")
	})

	_, err := m.client().GetStoreURL(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no device-service URL configured") {
		t.Errorf("snapd message not surfaced: %v", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("status code missing from error: %v", err)
	}
}

// ---- GetSystemInfo -------------------------------------------------------

func TestGetSystemInfo_ParsesFields(t *testing.T) {
	m := newMockSnapd(t)
	m.handle("GET", pathSystemInfo, func(w http.ResponseWriter, r *http.Request) {
		writeSync(t, w, http.StatusOK, map[string]any{
			"series":       "16",
			"version":      "2.65",
			"on-classic":   true,
			"managed":      false,
			"build-id":     "deadbeef",
			"system-mode":  "run",
			"confinement":  "strict",
		})
	})

	got, err := m.client().GetSystemInfo(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Series != "16" || got.Version != "2.65" || !got.OnClassic ||
		got.Managed || got.BuildID != "deadbeef" || got.SystemMode != "run" ||
		got.Confinement != "strict" {
		t.Errorf("system-info fields not parsed correctly: %+v", got)
	}
}

func TestGetSystemInfo_NetworkErrorWraps(t *testing.T) {
	// Point the client at a closed server to force a transport-level
	// error and check that we wrap it with the "request failed" prefix
	// (the observer relies on this prefix in its diagnostic line).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	c := NewClient(WithHTTPClient(srv.Client()), WithBaseURL(srv.URL))

	_, err := c.GetSystemInfo(context.Background())
	if err == nil {
		t.Fatal("expected error after server close")
	}
	if !strings.Contains(err.Error(), "snapd:") {
		t.Errorf("error missing snapd prefix: %v", err)
	}
}

// ---- IsSeeded ------------------------------------------------------------

func TestIsSeeded_True(t *testing.T) {
	m := newMockSnapd(t)
	m.handle("GET", "/v2/debug", func(w http.ResponseWriter, r *http.Request) {
		// pathDebugSeeding is "/v2/debug?aspect=seeding"; httptest's
		// mux matches on path only, so we verify the query separately.
		if r.URL.Query().Get("aspect") != "seeding" {
			t.Errorf("missing aspect=seeding query; got %q", r.URL.RawQuery)
		}
		writeSync(t, w, http.StatusOK, map[string]bool{"seeded": true})
	})

	ok, err := m.client().IsSeeded(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected seeded=true")
	}
}

func TestIsSeeded_False(t *testing.T) {
	m := newMockSnapd(t)
	m.handle("GET", "/v2/debug", func(w http.ResponseWriter, r *http.Request) {
		writeSync(t, w, http.StatusOK, map[string]bool{"seeded": false})
	})

	ok, err := m.client().IsSeeded(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected seeded=false")
	}
}

// ---- GetSerialAssertion --------------------------------------------------

func TestGetSerialAssertion_Success(t *testing.T) {
	m := newMockSnapd(t)
	m.handle("GET", pathSerial, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/x.ubuntu.assertion" {
			t.Errorf("wrong Accept header: %q", got)
		}
		w.Header().Set("Content-Type", "application/x.ubuntu.assertion")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sampleSerialAssertion)
	})

	got, err := m.client().GetSerialAssertion(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Serial() != "46923e6d-5d45-420d-905a-99a9e92493b4" {
		t.Errorf("serial: got %q", got.Serial())
	}
	if got.BrandID() != "generic" {
		t.Errorf("brand-id: got %q", got.BrandID())
	}
	if got.Raw == "" {
		t.Error("Raw should hold the verbatim assertion text")
	}
}

func TestGetSerialAssertion_404IsErrNoSerial(t *testing.T) {
	m := newMockSnapd(t)
	m.handle("GET", pathSerial, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no serial yet", http.StatusNotFound)
	})

	_, err := m.client().GetSerialAssertion(context.Background())
	if !errors.Is(err, ErrNoSerial) {
		t.Errorf("expected ErrNoSerial, got %v", err)
	}
}

func TestGetSerialAssertion_OtherStatusError(t *testing.T) {
	m := newMockSnapd(t)
	m.handle("GET", pathSerial, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	_, err := m.client().GetSerialAssertion(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrNoSerial) {
		t.Error("non-404 error must NOT match ErrNoSerial")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("status missing from error: %v", err)
	}
}

// ---- PostRegistrationData / ForgetRegistrationData -----------------------

func TestPostRegistrationData_Success(t *testing.T) {
	m := newMockSnapd(t)
	var gotBody map[string]json.RawMessage
	m.handle("POST", pathRegistrationData, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("missing/wrong Content-Type: %q", r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		writeSync(t, w, http.StatusOK, map[string]string{})
	})

	payload := &RegistrationPayload{
		Hardware:    json.RawMessage(`{"machine_id":"abc"}`),
		CollectedAt: "2025-04-29T12:00:00Z",
	}
	if err := m.client().PostRegistrationData(context.Background(), payload); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the wire shape: optional fields should be omitted, present
	// fields should be passed through verbatim.
	if _, ok := gotBody["hardware"]; !ok {
		t.Error("hardware missing from posted body")
	}
	if _, ok := gotBody["claim"]; ok {
		t.Error("claim should be omitted (omitempty) when nil")
	}
	if got := strings.TrimSpace(string(gotBody["collected_at"])); got != `"2025-04-29T12:00:00Z"` {
		t.Errorf("collected_at: got %s", got)
	}
}

func TestPostRegistrationData_SnapdErrorIsSurfaced(t *testing.T) {
	m := newMockSnapd(t)
	m.handle("POST", pathRegistrationData, func(w http.ResponseWriter, r *http.Request) {
		writeError(t, w, http.StatusBadRequest, "missing claim token")
	})

	err := m.client().PostRegistrationData(context.Background(), &RegistrationPayload{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "missing claim token") {
		t.Errorf("snapd message not surfaced: %v", err)
	}
}

func TestForgetRegistrationData_PostsForgetAction(t *testing.T) {
	m := newMockSnapd(t)
	var gotBody map[string]string
	m.handle("POST", pathRegistrationData, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		writeSync(t, w, http.StatusOK, map[string]string{})
	})

	if err := m.client().ForgetRegistrationData(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody["action"] != "forget" {
		t.Errorf(`expected {"action":"forget"}, got %v`, gotBody)
	}
}

// ---- Changes -------------------------------------------------------------

func TestGetChanges_PassesSelector(t *testing.T) {
	m := newMockSnapd(t)
	m.handle("GET", pathChanges, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("select") != "in-progress" {
			t.Errorf("missing select=in-progress: %q", r.URL.RawQuery)
		}
		writeSync(t, w, http.StatusOK, []map[string]any{
			{"id": "42", "kind": "become-operational", "summary": "Initialize device", "status": "Doing", "ready": false},
			{"id": "43", "kind": "auto-refresh", "summary": "Refresh snaps", "status": "Doing", "ready": false},
		})
	})

	changes, err := m.client().GetChanges(context.Background(), "in-progress")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(changes))
	}
	if changes[0].Kind != "become-operational" || changes[0].Status != "Doing" {
		t.Errorf("first change parsed wrong: %+v", changes[0])
	}
}

func TestGetChange_ParsesTasks(t *testing.T) {
	m := newMockSnapd(t)
	m.handle("GET", pathChanges+"/42", func(w http.ResponseWriter, r *http.Request) {
		writeSync(t, w, http.StatusOK, map[string]any{
			"id":      "42",
			"kind":    "become-operational",
			"summary": "Initialize device",
			"status":  "Error",
			"ready":   true,
			"err":     "request-serial failed",
			"tasks": []map[string]any{
				{
					"id":      "100",
					"kind":    "request-serial",
					"summary": "Request serial assertion",
					"status":  "Error",
					"log":     []string{"ERROR: backend rejected nonce", "WARNING: retry exhausted"},
				},
			},
		})
	})

	ch, err := m.client().GetChange(context.Background(), "42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ch.ID != "42" || ch.Status != "Error" || ch.Err != "request-serial failed" {
		t.Errorf("change fields parsed wrong: %+v", ch)
	}
	if len(ch.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(ch.Tasks))
	}
	if len(ch.Tasks[0].Log) != 2 || !strings.Contains(ch.Tasks[0].Log[0], "backend rejected") {
		t.Errorf("task log not parsed: %+v", ch.Tasks[0].Log)
	}
}

// ---- Warnings ------------------------------------------------------------

func TestGetWarnings_ParsesList(t *testing.T) {
	m := newMockSnapd(t)
	m.handle("GET", pathWarnings, func(w http.ResponseWriter, r *http.Request) {
		writeSync(t, w, http.StatusOK, []map[string]any{
			{
				"message":      `cannot install "foo": snap is blocked`,
				"first-added":  "2026-07-17T10:00:00Z",
				"last-added":   "2026-07-17T10:05:00Z",
				"expire-after": "672h0m0s",
			},
		})
	})

	warnings, err := m.client().GetWarnings(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(warnings))
	}
	if !strings.Contains(warnings[0].Message, "snap is blocked") {
		t.Errorf("message not parsed: %q", warnings[0].Message)
	}
	if warnings[0].FirstAdded.IsZero() || warnings[0].LastAdded.IsZero() {
		t.Errorf("timestamps not parsed: %+v", warnings[0])
	}
}

func TestGetWarnings_NullResultIsEmpty(t *testing.T) {
	// snapd returns `null` (not `[]`) when there are no warnings.
	m := newMockSnapd(t)
	m.handle("GET", pathWarnings, func(w http.ResponseWriter, r *http.Request) {
		writeSync(t, w, http.StatusOK, nil)
	})

	warnings, err := m.client().GetWarnings(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got %d", len(warnings))
	}
}

func TestGetChanges_ContextCancel(t *testing.T) {
	// Verify context propagation: a cancelled context stops the request
	// before the handler returns.
	m := newMockSnapd(t)
	gotRequest := make(chan struct{})
	m.handle("GET", pathChanges, func(w http.ResponseWriter, r *http.Request) {
		close(gotRequest)
		<-r.Context().Done() // hold the response until client disconnects
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := m.client().GetChanges(ctx, "")
		errCh <- err
	}()

	<-gotRequest
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Error("expected error from cancelled context, got nil")
		}
	case <-make(chan struct{}): // unreachable; placeholder for clarity
	}
}
