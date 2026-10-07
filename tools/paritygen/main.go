// SPDX-License-Identifier: Apache-2.0

// Command paritygen writes docs/reference/action-matrix.md, the action and
// read matrix (internal/parity), from the registry and this build's wiring
// (make gen).
package main

import (
	"fmt"
	"os"

	"github.com/ekaynac/auto-dev-cycle/internal/parity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: paritygen <output file>")
		os.Exit(64)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "paritygen:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	reg, err := vocab.Load()
	if err != nil {
		return err
	}
	actions, err := parity.Actions(reg)
	if err != nil {
		return err
	}
	b, err := parity.Markdown(actions, parity.Reads())
	if err != nil {
		return err
	}
	return os.WriteFile(out, b, 0o644)
}
