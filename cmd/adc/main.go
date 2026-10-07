// SPDX-License-Identifier: Apache-2.0

// Command adc is the M0 skeleton of the CLI.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ekaynac/auto-dev-cycle/internal/cli"
)

func main() {
	os.Exit(run())
}

// run is adc with an interrupt or a termination cancelling the command,
// so its cleanup still runs: a reserved --token-out with no token in it
// is removed (08 "Issued credentials"). The first signal cancels; once it
// has, a second one ends adc at once, as it would by default.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	return cli.Main(ctx, os.Args[1:], os.Stdout, os.Stderr, cli.ClientFromEnv(os.Getenv))
}
