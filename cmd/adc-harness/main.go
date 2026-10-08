// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Command adc-harness runs one attempt through the adapter host against a
// spawned adapter, on a throwaway state of its own (docs/design/m0/15 S1):
// `adc-harness run [flags] -- COMMAND...`. Every other command is the
// shared M0 skeleton entry.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/harness"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "run" {
		os.Exit(run())
	}
	os.Exit(entry.Main("adc-harness", os.Args[1:], os.Stdout, os.Stderr))
}

// run is the attempt with an interrupt, a termination or a hangup
// cancelling it, so the host still finalises the attempt and the throwaway
// state is removed. Once one has, a second ends the harness at once, as it
// would by default.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	return harness.Main(ctx, os.Args[2:], os.Stdout, os.Stderr, os.Getenv)
}
