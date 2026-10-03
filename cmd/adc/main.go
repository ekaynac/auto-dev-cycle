// SPDX-License-Identifier: Apache-2.0

// Command adc is the M0 skeleton of the CLI.
package main

import (
	"os"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

func main() {
	os.Exit(entry.Main("adc", os.Args[1:], os.Stdout, os.Stderr))
}
