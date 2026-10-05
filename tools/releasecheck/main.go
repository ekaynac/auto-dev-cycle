// SPDX-License-Identifier: Apache-2.0

// Command releasecheck refuses a release binary built with any build tags:
// release builds use none, and the adcmutation tag would put live mutation
// sites (internal/mutation) into one. It reads the build settings Go
// records in the binary, decoded (debug/buildinfo), so quoting in the
// recorded -tags value cannot hide a tag.
//
// Usage: go run ./tools/releasecheck FILE...
package main

import (
	"debug/buildinfo"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: releasecheck FILE...")
		os.Exit(2)
	}
	failed := false
	for _, f := range os.Args[1:] {
		if err := check(f); err != nil {
			fmt.Fprintf(os.Stderr, "releasecheck: %v\n", err)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}

// check refuses file unless it is a Go binary built without tags.
func check(file string) error {
	info, err := buildinfo.ReadFile(file)
	if err != nil {
		return fmt.Errorf("%s: no Go build information: %w", file, err)
	}
	for _, s := range info.Settings {
		if s.Key == "-tags" && s.Value != "" {
			return fmt.Errorf("%s was built with tags %q; a release build has none", file, s.Value)
		}
	}
	return nil
}
