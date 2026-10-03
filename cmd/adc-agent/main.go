// SPDX-License-Identifier: Apache-2.0

// Command adc-agent is the M0 skeleton of the dial-out capacity agent.
package main

import (
	"os"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

func main() {
	os.Exit(entry.Main("adc-agent", os.Args[1:], os.Stdout, os.Stderr))
}
