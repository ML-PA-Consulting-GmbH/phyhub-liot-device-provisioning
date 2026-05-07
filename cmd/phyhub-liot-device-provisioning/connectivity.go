package main

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// connectivityCheckInterval is how often the background watcher re-probes
// the Appstore. Slow enough to be cheap, fast enough that an operator
// noticing "no internet" doesn't sit through more than a minute of silence
// before seeing recovery.
const connectivityCheckInterval = 30 * time.Second

// connectivityProbeTimeout caps each individual HEAD request. Short on
// purpose: if the Appstore takes more than 5s to respond to a HEAD, the
// link is not in a useful state for registration anyway.
const connectivityProbeTimeout = 5 * time.Second

// connectivityProbeClient is the shared HTTP client for reachability
// probes. Reused across probes (negligible cost-saving, but consistent
// with the rest of the codebase).
var connectivityProbeClient = &http.Client{Timeout: connectivityProbeTimeout}

// startConnectivityWatch launches a background goroutine that periodically
// probes the Appstore URL. The first observation prints regardless of
// state ("OK" or "unavailable"); after that it only prints on transitions
// (going down, coming back up), so the console stays quiet during steady
// state.
//
// The watcher stops when ctx is cancelled.
func startConnectivityWatch(ctx context.Context, appstoreURL string) {
	go runConnectivityWatch(ctx, appstoreURL)
}

func runConnectivityWatch(ctx context.Context, appstoreURL string) {
	// We send HEAD to the bare Appstore URL. Any response (including 4xx)
	// means we reached the server; only network/timeout errors count as
	// "no connectivity". This deliberately conflates "internet" with
	// "Appstore reachable", which is the only kind of internet that
	// matters for the provisioning flow.
	probeURL := appstoreURL
	if u, err := url.Parse(appstoreURL); err == nil {
		// Ensure we don't have a trailing fragment / query messing with HEAD.
		u.Fragment = ""
		probeURL = u.String()
	}

	// firstCheck=true makes the first observation always print, so the
	// operator sees a baseline ("Internet OK" or "Internet unavailable")
	// even on a healthy boot. Subsequent observations print only on
	// transitions.
	firstCheck := true
	var lastReachable bool

	check := func() {
		reachable := probeReachable(ctx, probeURL)
		if firstCheck {
			if reachable {
				logf("Internet connection: OK (Appstore reachable)")
			} else {
				logf("No internet connection. Appstore unreachable. Registration will not complete until the network is back. Will keep checking.")
			}
			firstCheck = false
			lastReachable = reachable
			return
		}
		if reachable == lastReachable {
			return
		}
		if reachable {
			logf("Internet connection restored; Appstore is reachable again")
		} else {
			logf("Lost internet connection. Appstore is no longer reachable. Will keep checking.")
		}
		lastReachable = reachable
	}

	check()

	t := time.NewTicker(connectivityCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

// probeReachable does a single HEAD against url. Any HTTP response (200,
// 4xx, 5xx) means the network reached the server and counts as
// "reachable"; only transport-level failures (DNS, dial, timeout) count
// as "unreachable".
func probeReachable(ctx context.Context, url string) bool {
	cctx, cancel := context.WithTimeout(ctx, connectivityProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodHead, url, nil)
	if err != nil {
		return false
	}
	resp, err := connectivityProbeClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}
