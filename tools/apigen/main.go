// SPDX-License-Identifier: Apache-2.0

// Command apigen writes schemas/openapi.yaml from the action registry
// (make gen).
package main

import (
	"fmt"
	"os"

	"github.com/ekaynac/auto-dev-cycle/internal/apidoc"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: apigen <output file>")
		os.Exit(64)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "apigen:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	reg, err := vocab.Load()
	if err != nil {
		return err
	}
	b, err := apidoc.Generate(reg.Actions)
	if err != nil {
		return err
	}
	return os.WriteFile(out, b, 0o644)
}
