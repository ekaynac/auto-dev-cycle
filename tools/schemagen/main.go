// SPDX-License-Identifier: Apache-2.0

// Command schemagen writes the Go types for schemas/ into an output directory
// (make gen: gen/go/types). It removes stale generated files there, and fails
// on any schema shape outside the supported grammar.
package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/schemagen"
	"github.com/ekaynac/auto-dev-cycle/schemas"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: schemagen <output dir>")
		os.Exit(64)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "schemagen:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	paths, err := schemas.Paths()
	if err != nil {
		return err
	}
	in := map[string][]byte{}
	for _, p := range paths {
		if in[p], err = fs.ReadFile(schemas.FS(), p); err != nil {
			return err
		}
	}
	out, err := schemagen.Generate(in, filepath.Base(dir))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	stale, err := filepath.Glob(filepath.Join(dir, "*.gen.go"))
	if err != nil {
		return err
	}
	for _, f := range stale {
		if _, keep := out[filepath.Base(f)]; !keep {
			if err := os.Remove(f); err != nil {
				return err
			}
		}
	}
	for name, src := range out {
		if strings.ContainsRune(name, os.PathSeparator) {
			return fmt.Errorf("unexpected output name %q", name)
		}
		if err := os.WriteFile(filepath.Join(dir, name), src, 0o644); err != nil {
			return err
		}
	}
	return nil
}
