package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"phyhub-liot-device-provisioning/sdk/collector"
	"phyhub-liot-device-provisioning/sdk/snapd/observe"
	"phyhub-liot-device-provisioning/sdk/snapd/api"
)

func runBasic(args []string) {
	fs := flag.NewFlagSet("basic", flag.ExitOnError)
	observeInterval := fs.Duration("observe-interval", observe.DefaultInterval, "how often to poll snapd for registration progress after submission")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	api := api.NewClient()
	obs := observe.New(api,
		observe.WithInterval(*observeInterval),
		observe.WithOutput(os.Stdout),
	)

	// Wait until snapd is seeded before submitting. The POST would
	// succeed earlier, but registration cannot progress until seeding
	// completes; observing seeding first gives the user useful feedback
	// during the boot-up window.
	if _, err := obs.RunUntil(ctx, observe.Seeded); err != nil {
		log.Fatalf("error: snapd did not become ready: %v", err)
	}

	// Resolve the Appstore URL so the connectivity watcher has a target.
	// We don't otherwise need the URL in the basic flow (snapd handles
	// the actual registration HTTP), but probing the Appstore is the most
	// meaningful definition of "internet works" here.
	if storeURL, err := api.GetStoreURL(ctx); err == nil {
		startConnectivityWatch(ctx, storeURL)
	} else {
		log.Printf("warning: could not determine Appstore URL for connectivity watch: %v", err)
	}

	hw := collector.Collect(collector.WithOutput(os.Stderr))

	regPayload, err := buildPayload("", hw)
	if err != nil {
		log.Fatalf("error: build payload: %v", err)
	}

	if err := api.PostRegistrationData(ctx, regPayload); err != nil {
		log.Fatalf("error: submit registration data: %v", err)
	}
	// No "registration data submitted; observing..." line: the observer's
	// next event line (e.g. "Registering device with the L-IoT
	// Appstore...") makes the same thing visible, just better.

	final, err := obs.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("observation interrupted: %v", err)
	}
	printFinalBanner("basic", final)
}
