// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A binary without tags passes; any tags are refused, including a list Go
// records quoted (the round-2 bypass: -tags=adcmutation,x").
func TestCheck(t *testing.T) {
	gobin := filepath.Join(runtime.GOROOT(), "bin", "go")
	build := func(tags string) string {
		t.Helper()
		out := filepath.Join(t.TempDir(), "hello")
		args := []string{"build", "-trimpath", "-o", out}
		if tags != "" {
			args = append(args, "-tags", tags)
		}
		cmd := exec.Command(gobin, append(args, "./testdata/hello")...)
		cmd.Env = append(os.Environ(), "GOFLAGS=", "GOENV=off", "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build with tags %q: %v\n%s", tags, err, b)
		}
		return out
	}
	if err := check(build("")); err != nil {
		t.Fatalf("no tags: %v", err)
	}
	for _, tags := range []string{"adcmutation", `adcmutation,x"`, `x",adcmutation`, "other"} {
		if err := check(build(tags)); err == nil || !strings.Contains(err.Error(), "a release build has none") {
			t.Errorf("tags %q: %v", tags, err)
		}
	}
	notGo := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(notGo, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := check(notGo); err == nil {
		t.Error("a file without Go build information passed")
	}
}
