// SPDX-License-Identifier: Apache-2.0

// Command adcd is the controller daemon. `adcd run` starts it; every other
// command is the shared M0 skeleton entry.
package main

import (
	"os"

	"github.com/ekaynac/auto-dev-cycle/internal/daemon"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "run" {
		os.Exit(daemon.RunCommand(os.Args[2:], os.Stderr))
	}
	os.Exit(entry.Main("adcd", os.Args[1:], os.Stdout, os.Stderr))
}
