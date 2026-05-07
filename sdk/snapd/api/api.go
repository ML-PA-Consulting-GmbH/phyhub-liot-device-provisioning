// Package api is a thin client for snapd's local REST API over the
// /run/snapd.socket Unix socket. It exposes the endpoints that the L-IoT
// provisioning flows need: store-URL discovery, registration-data submission,
// and registration-status polling.
package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ErrNoSerial is returned by GetSerialAssertion when snapd reports no
// serial assertion exists yet (HTTP 404 with assertion-not-found).
var ErrNoSerial = errors.New("snapd: no serial assertion yet")


const (
	// SnapdSocket is the well-known path of snapd's local REST socket.
	SnapdSocket = "/run/snapd.socket"

	// defaultBaseURL is the host portion used for HTTP requests over the
	// Unix socket. The hostname is ignored by the unix-socket transport
	// but http.NewRequest needs one. Tests use httptest.Server's URL.
	defaultBaseURL = "http://localhost"

	// Local-API paths used by the provisioning flows.
	pathSystemInfo       = "/v2/system-info"
	pathSerial           = "/v2/model/serial"
	pathDebugSeeding     = "/v2/debug?aspect=seeding"
	pathChanges          = "/v2/changes"
	pathAppstoreURL      = "/v2/liot/appstore-url"
	pathRegistrationData = "/v2/liot/provisioning/registration-data"
)

// DefaultTimeout is the per-request HTTP timeout used when no
// WithTimeout option is supplied to NewClient.
//
// The default is generous (60s) because snapd briefly becomes
// unresponsive on the local socket while it is doing heavy work behind
// the state lock, most notably during the request-serial task, which
// makes blocking calls to the Appstore, signs requests with the TPM, and
// processes assertion responses. A short timeout would flap the observer
// between "reachable" and "unreachable" through every registration.
const DefaultTimeout = 60 * time.Second

// Client talks to snapd over its local Unix socket. Construct via NewClient;
// the zero value is unusable.
type Client struct {
	http    *http.Client
	baseURL string
}

// Option configures a Client. Options are applied left-to-right; later
// options override earlier ones.
type Option func(*clientConfig)

type clientConfig struct {
	socketPath string
	timeout    time.Duration
	httpClient *http.Client
	baseURL    string
}

// WithSocketPath overrides the snapd Unix-socket path. Useful for tests
// or for non-default snapd installations.
func WithSocketPath(path string) Option {
	return func(c *clientConfig) { c.socketPath = path }
}

// WithTimeout overrides the per-request HTTP timeout. See DefaultTimeout
// for the rationale behind the default.
func WithTimeout(d time.Duration) Option {
	return func(c *clientConfig) { c.timeout = d }
}

// WithHTTPClient supplies a fully-configured *http.Client, bypassing the
// built-in Unix-socket transport. Used by tests to target an
// httptest.Server, and available to callers that need custom transport
// (e.g. a remote proxy). When set, WithSocketPath and WithTimeout are
// ignored.
func WithHTTPClient(h *http.Client) Option {
	return func(c *clientConfig) { c.httpClient = h }
}

// WithBaseURL overrides the host portion of every request URL. The
// hostname is normally ignored by the unix-socket transport, so this is
// only useful when combined with WithHTTPClient (e.g. in tests).
func WithBaseURL(u string) Option {
	return func(c *clientConfig) { c.baseURL = u }
}

// NewClient returns a Client that, by default, dials /run/snapd.socket
// with a DefaultTimeout HTTP timeout. Override either via options.
func NewClient(opts ...Option) *Client {
	cfg := clientConfig{
		socketPath: SnapdSocket,
		timeout:    DefaultTimeout,
		baseURL:    defaultBaseURL,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	httpClient := cfg.httpClient
	if httpClient == nil {
		httpClient = unixSocketHTTPClient(cfg.socketPath, cfg.timeout)
	}
	return &Client{http: httpClient, baseURL: cfg.baseURL}
}

// snapdEnvelope is the outer wrapper of every snapd REST response.
type snapdEnvelope struct {
	Type       string          `json:"type"`
	StatusCode int             `json:"status-code"`
	Result     json.RawMessage `json:"result"`
}

// snapdErrorResult mirrors the result body snapd returns inside an error
// envelope. Used to surface a useful error message instead of the bare
// HTTP status code.
type snapdErrorResult struct {
	Message string `json:"message"`
	Kind    string `json:"kind,omitempty"`
}

// appstoreURLResp is the result-envelope shape of GET /v2/liot/appstore-url.
type appstoreURLResp struct {
	URL string `json:"url"`
}

// GetStoreURL returns the Appstore base URL configured for this device, via
// the L-IoT-specific `GET /v2/liot/appstore-url` endpoint. snapd resolves the
// URL the same way it would for its own serial-request flow (gadget config
// "device-service.url" first, fallback to baked-in defaults).
//
// We deliberately do NOT use `GET /v2/find?q=get-snapstore-url` here:
// snapd gates that path on the device having a serial assertion, returning
// HTTP 500 "no device serial yet" before registration has completed, which
// is precisely when the claiming-token flow needs the URL.
func (c *Client) GetStoreURL(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+pathAppstoreURL, nil)
	if err != nil {
		return "", fmt.Errorf("snapd: build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("snapd: request failed: %w", err)
	}
	defer resp.Body.Close()

	var env snapdEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return "", fmt.Errorf("snapd: decode response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errBody snapdErrorResult
		// Best-effort: if the result envelope isn't shaped like a snapd
		// error body, fall through to the generic "status N" message
		// below. There's nothing useful to surface from the decode error.
		_ = json.Unmarshal(env.Result, &errBody)
		if errBody.Message != "" {
			return "", fmt.Errorf("snapd: %s (status %d)", errBody.Message, resp.StatusCode)
		}
		return "", fmt.Errorf("snapd: GET /v2/liot/appstore-url returned status %d", resp.StatusCode)
	}

	var parsed appstoreURLResp
	if err := json.Unmarshal(env.Result, &parsed); err != nil {
		return "", fmt.Errorf("snapd: decode result: %w", err)
	}
	if parsed.URL == "" {
		return "", fmt.Errorf("snapd: empty Appstore URL")
	}
	// Normalise: strip trailing slashes so callers can safely concatenate
	// a path like "/device/v3/..." without producing a "//".
	return strings.TrimRight(parsed.URL, "/"), nil
}

// RegistrationPayload is the body POSTed to snapd's
// /v2/liot/provisioning/registration-data endpoint. Fields owned by snapd
// (format_version, nonce, snap, attestation) are intentionally absent: snapd
// injects authoritative values at assembly time. See the L-IoT registration
// format specification for the resulting wire shape.
type RegistrationPayload struct {
	Claim       json.RawMessage `json:"claim,omitempty"`
	Hardware    json.RawMessage `json:"hardware,omitempty"`
	Software    json.RawMessage `json:"software,omitempty"`
	Collector   json.RawMessage `json:"collector,omitempty"`
	CollectedAt string          `json:"collected_at,omitempty"`
}

// PostRegistrationData submits the partial registration payload to snapd.
// Returns nil on success (HTTP 200) and a descriptive error on any failure,
// including snapd-side validation errors.
func (c *Client) PostRegistrationData(ctx context.Context, payload *RegistrationPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("snapd: marshal payload: %w", err)
	}
	return c.postRegistration(ctx, body)
}

// ForgetRegistrationData asks snapd to drop any stored partial payload and
// abort the in-flight registration. Used when the claim TTL has elapsed
// without registration completing; the tool then reboots the device so a
// fresh attempt starts cleanly on the next boot. See the L-IoT registration
// endpoint's `action: "forget"` mode.
func (c *Client) ForgetRegistrationData(ctx context.Context) error {
	return c.postRegistration(ctx, []byte(`{"action":"forget"}`))
}

// postRegistration is the shared transport used by both PostRegistrationData
// and ForgetRegistrationData.
func (c *Client) postRegistration(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+pathRegistrationData, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("snapd: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("snapd: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}

	// Try to surface the snapd error message from the result envelope.
	var env snapdEnvelope
	if jerr := json.NewDecoder(resp.Body).Decode(&env); jerr == nil {
		var errBody struct {
			Message string `json:"message"`
		}
		// Best-effort: if Result isn't an error envelope, the generic
		// "status N" line below is still emitted.
		_ = json.Unmarshal(env.Result, &errBody)
		if errBody.Message != "" {
			return fmt.Errorf("snapd: %s (status %d)", errBody.Message, resp.StatusCode)
		}
	}
	return fmt.Errorf("snapd: POST registration-data returned status %d", resp.StatusCode)
}

// SystemInfo is a small, observation-only subset of /v2/system-info that
// the provisioning flows surface in their dashboards. Other fields exposed
// by snapd are intentionally not modelled; this is not a general-purpose
// system-info client.
type SystemInfo struct {
	Series      string `json:"series,omitempty"`
	Version     string `json:"version,omitempty"`
	OnClassic   bool   `json:"on-classic"`
	Managed     bool   `json:"managed"`
	BuildID     string `json:"build-id,omitempty"`
	SystemMode  string `json:"system-mode,omitempty"`
	Confinement string `json:"confinement,omitempty"`
}

// GetSystemInfo returns snapd's current view of the system. Used by the
// flows primarily as a reachability check; specific signals like "is snapd
// seeded?" live behind dedicated methods (see IsSeeded), because not every
// piece of state we care about is exposed via /v2/system-info.
func (c *Client) GetSystemInfo(ctx context.Context) (SystemInfo, error) {
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+pathSystemInfo, nil)
	if rerr != nil {
		return SystemInfo{}, fmt.Errorf("snapd: build request: %w", rerr)
	}
	resp, derr := c.http.Do(req)
	if derr != nil {
		return SystemInfo{}, fmt.Errorf("snapd: request failed: %w", derr)
	}
	defer resp.Body.Close()

	var env snapdEnvelope
	if jerr := json.NewDecoder(resp.Body).Decode(&env); jerr != nil {
		return SystemInfo{}, fmt.Errorf("snapd: decode response: %w", jerr)
	}
	if resp.StatusCode != http.StatusOK || env.Type != "sync" {
		return SystemInfo{}, fmt.Errorf("snapd: unexpected response type=%s status=%d", env.Type, resp.StatusCode)
	}
	var parsed SystemInfo
	if jerr := json.Unmarshal(env.Result, &parsed); jerr != nil {
		return SystemInfo{}, fmt.Errorf("snapd: decode system-info: %w", jerr)
	}
	return parsed, nil
}

// seedingResp is the result-envelope shape of GET /v2/debug?aspect=seeding.
// We only care about Seeded; other fields (preseed times, seed times) are
// not modelled.
type seedingResp struct {
	Seeded bool `json:"seeded"`
}

// IsSeeded reports whether snapd has finished initial seeding.
//
// Note: this hits /v2/debug?aspect=seeding rather than /v2/system-info,
// because /v2/system-info does NOT include the seeded flag at all (snapd
// only exposes it via the debug-seeding aspect). The "/debug" namespace
// is misleading: it's a stable observation surface and the source for
// `snap debug seeding`.
func (c *Client) IsSeeded(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+pathDebugSeeding, nil)
	if err != nil {
		return false, fmt.Errorf("snapd: build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("snapd: request failed: %w", err)
	}
	defer resp.Body.Close()

	var env snapdEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return false, fmt.Errorf("snapd: decode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || env.Type != "sync" {
		return false, fmt.Errorf("snapd: unexpected response type=%s status=%d", env.Type, resp.StatusCode)
	}
	var parsed seedingResp
	if err := json.Unmarshal(env.Result, &parsed); err != nil {
		return false, fmt.Errorf("snapd: decode seeding info: %w", err)
	}
	return parsed.Seeded, nil
}

// SerialAssertion is the parsed view of the device serial assertion
// returned by GET /v2/model/serial. Raw is the verbatim assertion text
// (handy for debugging); Headers holds the single-line key/value pairs
// from the assertion header section.
type SerialAssertion struct {
	Raw     string
	Headers map[string]string
}

// Serial returns the "serial" header (the OS-Serial UUID assigned to
// this device by the Appstore).
func (s *SerialAssertion) Serial() string { return s.Headers["serial"] }

// BrandID returns the "brand-id" header.
func (s *SerialAssertion) BrandID() string { return s.Headers["brand-id"] }

// Model returns the "model" header.
func (s *SerialAssertion) Model() string { return s.Headers["model"] }

// GetSerialAssertion fetches the device's current serial assertion from
// snapd via GET /v2/model/serial. Returns ErrNoSerial when snapd reports
// no serial assertion exists yet (HTTP 404).
//
// The presence of a serial assertion is the universal signal that the
// device is registered; it works on both upstream snapd and our patched
// snapd. The L-IoT registration-data endpoint provides richer in-flight
// status when available, but the serial is the ground truth.
func (c *Client) GetSerialAssertion(ctx context.Context) (*SerialAssertion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+pathSerial, nil)
	if err != nil {
		return nil, fmt.Errorf("snapd: build request: %w", err)
	}
	req.Header.Set("Accept", "application/x.ubuntu.assertion")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("snapd: request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		body, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			return nil, fmt.Errorf("snapd: read assertion body: %w", rerr)
		}
		raw := string(body)
		headers, perr := parseAssertionHeaders(raw)
		if perr != nil {
			return nil, fmt.Errorf("snapd: parse serial assertion: %w", perr)
		}
		return &SerialAssertion{Raw: raw, Headers: headers}, nil
	case http.StatusNotFound:
		return nil, ErrNoSerial
	default:
		return nil, fmt.Errorf("snapd: GET /v2/model/serial returned status %d", resp.StatusCode)
	}
}

// parseAssertionHeaders extracts the single-line key/value pairs from the
// header section of an assertion. The header section runs until the first
// empty line; multi-line values (continuation lines starting with
// whitespace, e.g. the device-key block) are captured but rarely needed by
// the provisioning flows. We only care about scalar fields like serial,
// brand-id and model.
func parseAssertionHeaders(s string) (map[string]string, error) {
	out := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(s))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var lastKey string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break // end of header section
		}
		// Continuation line (starts with whitespace): append to the
		// last key's value. Rare and irrelevant to our scalar fields.
		if line[0] == ' ' || line[0] == '\t' {
			if lastKey != "" {
				out[lastKey] += "\n" + strings.TrimLeft(line, " \t")
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		out[k] = v
		lastKey = k
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Change is the subset of snapd's `/v2/changes` and `/v2/changes/{id}`
// response we use for diagnostics. Mirrors snapd's wire format directly so
// it stays compatible with `snap changes` / `snap tasks` output.
type Change struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Summary   string    `json:"summary"`
	Status    string    `json:"status"`
	Ready     bool      `json:"ready"`
	Err       string    `json:"err,omitempty"`
	SpawnTime time.Time `json:"spawn-time"`
	ReadyTime time.Time `json:"ready-time,omitempty"`
	Tasks     []Task    `json:"tasks,omitempty"`
}

// Task is a single task within a Change.
type Task struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Summary   string    `json:"summary"`
	Status    string    `json:"status"`
	Log       []string  `json:"log,omitempty"`
	SpawnTime time.Time `json:"spawn-time"`
	ReadyTime time.Time `json:"ready-time,omitempty"`
}

// GetChanges returns the list of snapd changes matching the selector.
// Valid selectors: "in-progress", "ready", "all" (default in snapd is
// "in-progress" if the parameter is empty). Tasks are NOT included in
// this response; use GetChange(id) for the full task list.
func (c *Client) GetChanges(ctx context.Context, selector string) ([]Change, error) {
	url := c.baseURL + pathChanges
	if selector != "" {
		url += "?select=" + selector
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("snapd: build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("snapd: request failed: %w", err)
	}
	defer resp.Body.Close()

	var env snapdEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("snapd: decode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || env.Type != "sync" {
		return nil, fmt.Errorf("snapd: unexpected response type=%s status=%d", env.Type, resp.StatusCode)
	}
	var changes []Change
	if err := json.Unmarshal(env.Result, &changes); err != nil {
		return nil, fmt.Errorf("snapd: decode changes: %w", err)
	}
	return changes, nil
}

// GetChange fetches a single change by ID, including its full task list
// (and per-task log lines).
func (c *Client) GetChange(ctx context.Context, id string) (*Change, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+pathChanges+"/"+id, nil)
	if err != nil {
		return nil, fmt.Errorf("snapd: build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("snapd: request failed: %w", err)
	}
	defer resp.Body.Close()

	var env snapdEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("snapd: decode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || env.Type != "sync" {
		return nil, fmt.Errorf("snapd: unexpected response type=%s status=%d", env.Type, resp.StatusCode)
	}
	var change Change
	if err := json.Unmarshal(env.Result, &change); err != nil {
		return nil, fmt.Errorf("snapd: decode change: %w", err)
	}
	return &change, nil
}

func unixSocketHTTPClient(socketPath string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
	}
}
