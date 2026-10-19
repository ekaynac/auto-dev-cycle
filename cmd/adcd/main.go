// SPDX-License-Identifier: Apache-2.0

// Command adcd is the controller daemon. `adcd run` starts it; its offline
// recovery commands (daemon.Commands, 14 R7, R8) run while it is stopped;
// every other command is the shared M0 skeleton entry.
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
	if len(os.Args) > 2 {
		if cmd, ok := daemon.Commands[[2]string{os.Args[1], os.Args[2]}]; ok {
			os.Exit(cmd(os.Args[3:], os.Stderr))
		}
	}
	os.Exit(entry.Main("adcd", os.Args[1:], os.Stdout, os.Stderr))
}
