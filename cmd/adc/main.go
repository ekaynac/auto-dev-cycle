// SPDX-License-Identifier: Apache-2.0

// Command adc is the M0 skeleton of the CLI.
package main

import (
	"context"
	"os"

	"github.com/ekaynac/auto-dev-cycle/internal/cli"
)

func main() {
	os.Exit(cli.Main(context.Background(), os.Args[1:], os.Stdout, os.Stderr, cli.ClientFromEnv(os.Getenv)))
}
