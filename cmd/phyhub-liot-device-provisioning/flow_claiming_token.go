package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"phyhub-liot-device-provisioning/sdk/claim"
	"phyhub-liot-device-provisioning/sdk/collector"
	"phyhub-liot-device-provisioning/sdk/snapd/observe"
	"phyhub-liot-device-provisioning/sdk/payload"
	"phyhub-liot-device-provisioning/sdk/snapd/api"
)

func runClaimingToken(args []string) {
	fs := flag.NewFlagSet("claiming-token", flag.ExitOnError)
	tokenFile := fs.String("token-file", "/var/lib/snapd/claiming-token", "path to persist the claiming token across reboots")
	resetToken := fs.Bool("reset-token", false, "force generation of a new claiming token")
	pollInterval := fs.Duration("poll-interval", claim.DefaultPollInterval, "how often to poll the Appstore claim-status endpoint")
	observeInterval := fs.Duration("observe-interval", observe.DefaultInterval, "how often to poll snapd for registration progress after submission")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	api := api.NewClient()
	obs := observe.New(api,
		observe.WithInterval(*observeInterval),
		observe.WithOutput(os.Stdout),
	)

	// Phase 1: wait until snapd is seeded before showing the token. There
	// is no point asking the user to claim a device whose snapd cannot yet
	// resolve a store URL, and surfacing seeding progress reassures the
	// user that the device is alive during the boot-up window.
	if _, err := obs.RunUntil(ctx, observe.Seeded); err != nil {
		log.Fatalf("error: snapd did not become ready: %v", err)
	}

	storeURL, err := api.GetStoreURL(ctx)
	if err != nil {
		log.Fatalf("error: could not determine Appstore URL: %v", err)
	}

	// Background watcher: probes the Appstore periodically and prints on
	// reachability transitions. Useful when the device boots without
	// network and the operator sits in front of a console wondering why
	// nothing is happening.
	startConnectivityWatch(ctx, storeURL)

	// strings.TrimRight to defensively collapse any trailing slashes
	// before joining the claim-status path. snapd's baseURL constants
	// happen to end with "/", but we don't want a "//" in the URL we
	// submit either way.
	statusURL := strings.TrimRight(storeURL, "/") + "/device/v3/provisioning/claim/status"

	tok, err := claim.LoadOrGenerate(*tokenFile, *resetToken)
	if err != nil {
		log.Printf("warning: %v", err)
	}

	printTokenBanner(tok)
	fmt.Println("  Press Alt+R at any time to reset the token and reboot.")
	fmt.Println()

	// Run a console watcher in the background. If the operator presses
	// Alt+R (e.g. they typed the wrong token into the Appstore and want
	// to start over), trigger cleanupAndReboot from a separate goroutine
	// (the system will go down regardless of where the main flow is).
	resetReq := watchForReset(ctx)
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-resetReq:
			fmt.Println()
			logf("Reset requested by operator (Alt+R).")
			cleanupAndReboot(api, *tokenFile)
			os.Exit(0)
		}
	}()

	poller := &claim.Poller{Interval: *pollInterval, Output: os.Stdout}
	claimed, ttl, err := poller.Run(ctx, statusURL, tok)
	if err != nil {
		log.Printf("Provisioning interrupted: %v", err)
		return
	}
	if !claimed {
		// claim.PollUntilClaimed already announced "Token expired before
		// registration completed". Clear local + remote state and reboot
		// so the next boot starts cleanly with a fresh token.
		cleanupAndReboot(api, *tokenFile)
		return
	}

	// Token is claimed; we have `ttl` to complete registration before the
	// backend marks it expired.
	deadline := time.Now().Add(ttl)
	dctx, dcancel := context.WithDeadline(ctx, deadline)
	defer dcancel()

	// Heartbeat: every 30 minutes, log how much TTL is left so the operator
	// knows registration is still inside the window.
	go ttlHeartbeat(dctx, deadline)

	hw := collector.Collect(collector.WithOutput(os.Stderr))
	regPayload, err := buildPayload(tok, hw)
	if err != nil {
		log.Fatalf("error: build payload: %v", err)
	}
	if err := api.PostRegistrationData(dctx, regPayload); err != nil {
		log.Fatalf("error: submit registration data: %v", err)
	}

	final, oerr := obs.Run(dctx)

	if final.Serial != nil {
		printFinalBanner("claiming-token", final)
		return
	}
	if ctx.Err() != nil {
		// Parent context cancelled (Ctrl+C / SIGTERM).
		return
	}
	if errors.Is(oerr, context.DeadlineExceeded) || errors.Is(dctx.Err(), context.DeadlineExceeded) {
		logf("Token expired before registration completed.")
		cleanupAndReboot(api, *tokenFile)
		return
	}
	if oerr != nil {
		log.Printf("observation interrupted: %v", oerr)
	}
	printFinalBanner("claiming-token", final)
}

// cleanupAndReboot wipes the per-attempt provisioning state (snapd's
// partial payload + the local token file), gives the user a 5-second
// countdown so the message lands, then reboots the device.
//
// Used both on TTL expiry (automatic) and when the operator presses Alt+R
// (manual). Does not return on success: the system goes down. On failure
// to reboot it logs and exits 1.
func cleanupAndReboot(api *api.Client, tokenFile string) {
	logf("Cleaning up and rebooting to start over...")

	// Tell snapd to drop the partial payload + probe cache + abort the
	// in-flight become-operational change.
	forgetCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := api.ForgetRegistrationData(forgetCtx); err != nil {
		logf("warning: could not ask Snapd to forget the partial payload: %v", err)
	}
	cancel()

	// Drop the local token file so the next boot generates a fresh token.
	if err := os.Remove(tokenFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		logf("warning: could not remove token file %s: %v", tokenFile, err)
	}

	fmt.Println()
	for i := 5; i > 0; i-- {
		fmt.Printf("  Rebooting in %d...\n", i)
		time.Sleep(1 * time.Second)
	}
	fmt.Println()

	// systemctl reboot is the systemd-aware way; legacy `reboot(8)` as
	// a fallback. Absolute paths are used so a tampered or unexpected PATH
	// can't redirect the reboot to a different binary. systemctl exits
	// immediately and the system goes down moments later, so we just sleep
	// until that happens.
	for _, path := range []string{"/usr/bin/systemctl", "/bin/systemctl"} {
		if err := exec.Command(path, "reboot").Run(); err == nil {
			time.Sleep(60 * time.Second)
			return
		}
	}
	if err := exec.Command("/sbin/reboot").Run(); err != nil {
		logf("error: reboot command failed: %v", err)
		os.Exit(1)
	}
	time.Sleep(60 * time.Second)
}

// ttlHeartbeat prints the remaining TTL every 30 minutes while ctx is alive.
// Keeps the operator informed during long observation windows without
// spamming the console.
func ttlHeartbeat(ctx context.Context, deadline time.Time) {
	const interval = 30 * time.Minute
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			remaining := time.Until(deadline).Round(time.Minute)
			if remaining > 0 {
				logf("Token TTL: %s remaining", remaining)
			}
		}
	}
}

// logf writes a single timestamped line. The `[HH:MM:SS]` stamp matches the
// observer's output; the log package's default "YYYY/MM/DD HH:MM:SS" prefix
// is suppressed in main.go's init() via log.SetFlags(0).
func logf(format string, args ...any) {
	log.Printf("[%s] "+format, append([]any{time.Now().Format("15:04:05")}, args...)...)
}

// printFinalBanner prints a closing summary based on the last observed
// snapshot. The serial assertion drives the success banner (and gives us
// the OS-Serial UUID); the tool then returns from main and the process
// exits.
//
// Also emits a structured "DONE" event line for automation. Decorative
// banner goes to stdout (and the journal); operators can grep for the DONE
// line.
func printFinalBanner(flow string, s observe.Snapshot) {
	if s.Serial != nil {
		fmt.Println()
		fmt.Println(boxBorder(bannerWidth))
		fmt.Println(boxLine("Provisioning complete", bannerWidth))
		fmt.Println(boxBorder(bannerWidth))
		fmt.Println()
		fmt.Printf("  OS-Serial: %s\n", fallback(s.Serial.Serial(), "(missing)"))
		fmt.Printf("  Brand:     %s\n", fallback(s.Serial.BrandID(), "(unknown)"))
		fmt.Printf("  Model:     %s\n", fallback(s.Serial.Model(), "(unknown)"))
		fmt.Println()
		fmt.Printf("DONE flow=%s version=%s result=registered os-serial=%s ended=%s\n",
			flow, version,
			fallback(s.Serial.Serial(), "unknown"),
			time.Now().UTC().Format(time.RFC3339))
		return
	}

	fmt.Println()
	fmt.Println("Provisioning ended before the device was registered.")
	fmt.Println("Inspect 'snap changes' or 'journalctl -u snapd' for diagnostic detail.")
	fmt.Printf("DONE flow=%s version=%s result=incomplete ended=%s\n",
		flow, version, time.Now().UTC().Format(time.RFC3339))
}

// printTokenBanner shows the freshly-generated token in a centered box.
// No TTL is shown here; the TTL only starts when the user enters the
// token in the Appstore, which we can't predict. After the token is
// claimed, ttlHeartbeat prints `Token TTL: ...` lines every 30 minutes.
func printTokenBanner(tok string) {
	fmt.Println()
	fmt.Println(boxBorder(bannerWidth))
	fmt.Println(boxLine("Enter this token in the L-IoT", bannerWidth))
	fmt.Println(boxLine("Appstore to bind this device", bannerWidth))
	fmt.Println(boxLine("to your account:", bannerWidth))
	fmt.Println(boxLine("", bannerWidth))
	fmt.Println(boxLine(tok, bannerWidth))
	fmt.Println(boxLine("", bannerWidth))
	fmt.Println(boxBorder(bannerWidth))
	fmt.Println()
}

// buildPayload assembles a api.RegistrationPayload from the collected
// hardware info plus an optional claiming token. The token is embedded in the
// "claim" field; pass an empty token for the basic flow.
func buildPayload(token string, hw collector.HardwareInfo) (*api.RegistrationPayload, error) {
	claimField, err := payload.ClaimJSON(token)
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	hwField, err := payload.HardwareJSON(hw)
	if err != nil {
		return nil, fmt.Errorf("hardware: %w", err)
	}
	swField, err := payload.SoftwareJSON()
	if err != nil {
		return nil, fmt.Errorf("software: %w", err)
	}
	// binary_sha256 is best-effort: hashing failure logs a warning and we
	// emit the collector field without it.
	binarySHA, err := payload.SelfBinarySHA256()
	if err != nil {
		log.Printf("warning: cannot hash self for collector.binary_sha256: %v", err)
		binarySHA = ""
	}
	collectorField, err := payload.CollectorJSON(collectorName, version, binarySHA)
	if err != nil {
		return nil, fmt.Errorf("collector: %w", err)
	}
	return &api.RegistrationPayload{
		Claim:       claimField,
		Hardware:    hwField,
		Software:    swField,
		Collector:   collectorField,
		CollectedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
