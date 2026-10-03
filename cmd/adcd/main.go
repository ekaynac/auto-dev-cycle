// SPDX-License-Identifier: Apache-2.0

// Command adcd is the M0 skeleton of the controller daemon.
package main

import (
	"os"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

func main() {
	os.Exit(entry.Main("adcd", os.Args[1:], os.Stdout, os.Stderr))
}
