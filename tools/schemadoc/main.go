// SPDX-License-Identifier: Apache-2.0

// Command schemadoc writes docs/reference/schemas from the embedded schemas
// (make gen). The output directory holds only generated pages: it is
// replaced whole.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ekaynac/auto-dev-cycle/internal/schemadoc"
	"github.com/ekaynac/auto-dev-cycle/schemas"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: schemadoc <output dir>")
		os.Exit(64)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "schemadoc:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	files, err := schemas.Paths()
	if err != nil {
		return err
	}
	pages, err := schemadoc.Render(schemas.FS(), files)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for _, p := range pages {
		if err := os.WriteFile(filepath.Join(out, p.Name), []byte(p.Content), 0o644); err != nil {
			return err
		}
	}
	return nil
}
