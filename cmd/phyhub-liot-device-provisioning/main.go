// liot-provisioning: L-IoT device provisioning tool.
//
// Collects device facts and submits them to snapd's local registration API.
// The first positional argument selects the flow:
//
//	liot-provisioning claiming-token [flags]   claim-token flow
//	liot-provisioning basic          [flags]   collect + POST, no token
//
// Run "liot-provisioning <flow> -h" for flow-specific flags.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"syscall"
	"time"

	"phyhub-liot-device-provisioning/sdk/snapd/api"
)

func init() {
	// We do our own [HH:MM:SS] stamping in messages, so drop the
	// `log` package's default "YYYY/MM/DD HH:MM:SS " prefix so all
	// output reads consistently.
	log.SetFlags(0)
}

// version is injected at build time via -ldflags "-X main.version=<ver>".
var version = "dev"

const collectorName = "liot-provisioning"

// alreadyRegisteredTimeout is how long we wait for snapd to answer the
// "are we registered?" probe before giving up and proceeding with the flow.
// Short on purpose: if snapd isn't even up yet, the flow's observer is the
// right place to wait.
const alreadyRegisteredTimeout = 5 * time.Second

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	flow := os.Args[1]
	args := os.Args[2:]

	switch flow {
	case "claiming-token", "basic":
		// If the device already has a serial assertion, provisioning
		// is complete from a previous boot, so exit silently and
		// don't spam the console on every subsequent boot.
		if alreadyRegistered() {
			os.Exit(0)
		}
		printStartBanner(flow)
		if flow == "claiming-token" {
			runClaimingToken(args)
		} else {
			runBasic(args)
		}
	case "-h", "--help", "help":
		usage()
	case "--version", "-v":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown flow %q\n\n", flow)
		usage()
		os.Exit(2)
	}
}

// alreadyRegistered queries snapd for a serial assertion: the universal
// "device is registered" signal that works on both upstream snapd and our
// patched build. Returns true only when snapd is reachable AND has stored
// a non-empty serial. Every other outcome (snapd not up yet, no serial yet,
// transient error) returns false so the flow proceeds; the observer will
// surface real problems with appropriate detail.
func alreadyRegistered() bool {
	ctx, cancel := context.WithTimeout(context.Background(), alreadyRegisteredTimeout)
	defer cancel()

	serial, err := api.NewClient().GetSerialAssertion(ctx)
	if err != nil || serial == nil {
		return false
	}
	return serial.Serial() != ""
}

// bannerWidth is the inner width (between the two `|`) of every banner the
// tool prints. Centralised so every banner aligns to the same width.
const bannerWidth = 38

// printStartBanner is shown once at the top of every "real" run (i.e. the
// device is not already registered). The decorative banner is written *only*
// to /dev/console so it does not pollute the journal. A second, structured,
// machine-friendly "START" line goes to stdout (and therefore to the
// journal too) so automation can grep for it.
//
// Best-effort: if /dev/console isn't writable (running as non-root during
// development), the decorative banner is silently skipped; the structured
// line still goes out.
func printStartBanner(flow string) {
	banner := "\n" +
		boxBorder(bannerWidth) + "\n" +
		boxLine("L-IoT  Provisioning", bannerWidth) + "\n" +
		boxLine("Flow: "+flow+"   v"+version, bannerWidth) + "\n" +
		boxBorder(bannerWidth) + "\n\n"

	if f, err := os.OpenFile("/dev/console", os.O_WRONLY|syscall.O_NOCTTY, 0); err == nil {
		_, _ = f.WriteString(banner)
		_ = f.Close()
	}

	// Structured START event for automation. Everything after this is
	// human-readable narrative; STOP/DONE event is printed by flows on
	// completion. Keep keys short and stable.
	fmt.Printf("START flow=%s version=%s started=%s\n",
		flow, version, time.Now().UTC().Format(time.RFC3339))
}

func usage() {
	fmt.Fprintf(os.Stderr, `liot-provisioning %s: L-IoT device provisioning

Usage:
  liot-provisioning <flow> [flags]

Flows:
  claiming-token  Generate a claiming token, display it on the console,
                  poll the Appstore until the user claims it, then submit
                  collected device facts to snapd.

  basic           Collect device facts and submit them to snapd. No token.

Run "liot-provisioning <flow> -h" for flow-specific flags.
`, version)
}
