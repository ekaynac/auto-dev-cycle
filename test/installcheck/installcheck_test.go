// SPDX-License-Identifier: Apache-2.0

// Package installcheck tests scripts/install-check.sh against fake builds:
// every refusal it claims, and that the installed binaries run outside the
// source tree with a scrubbed environment.
package installcheck

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const goodSet = "sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"

type fake struct{ binary, platform, commit, schemaSet string }

// dist writes a fake dist/<os>-<arch>/ whose binaries print version JSON,
// and refuse to answer when the test's marker variable leaked into their
// environment or they run in the caller's directory.
func dist(t *testing.T, fakes []fake) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "linux-"+runtime.GOARCH)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	caller, _ := os.Getwd()
	repo, _ := filepath.Abs("../..")
	home := os.Getenv("HOME")
	for _, f := range fakes {
		script := fmt.Sprintf(`#!/bin/sh
[ -n "$ADC_INSTALLCHECK_MARKER" ] && { echo "the environment leaked" >&2; exit 3; }
case "$(pwd)" in %q|%q|%q/*) echo "ran in the source tree" >&2; exit 3 ;; esac
[ "$HOME" = %q ] && { echo "ran with the caller's HOME" >&2; exit 3; }
[ "$1 $2" = "version --json" ] || exit 4
printf '{"binary":"%s","version":"v","commit":"%s","schema_set":"%s","go_version":"go","platform":"%s"}\n'
`, caller, repo, repo, home, f.binary, f.commit, f.schemaSet, f.platform)
		if err := os.WriteFile(filepath.Join(dir, f.binary), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func good() []fake {
	p := "linux/" + runtime.GOARCH
	return []fake{{"adcd", p, "c1c1c1c", goodSet}, {"adc", p, "c1c1c1c", goodSet}, {"adc-agent", p, "c1c1c1c", goodSet}}
}

func run(t *testing.T, distDir, prefix string, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{"../../scripts/install-check.sh", "--dist", distDir}, extra...)
	cmd := exec.Command("bash", append(args, prefix)...)
	cmd.Env = append(os.Environ(), "ADC_INSTALLCHECK_MARKER=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallCheck(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the install check supports Linux hosts")
	}
	prefix := filepath.Join(t.TempDir(), "prefix")
	out, err := run(t, dist(t, good()), prefix, "--expect", goodSet)
	if err != nil || !strings.Contains(out, "installed into") {
		t.Fatalf("a good build: %v\n%s", err, out)
	}
	for _, b := range []string{"adcd", "adc", "adc-agent"} {
		if fi, err := os.Lstat(filepath.Join(prefix, "bin", b)); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o755 {
			t.Errorf("%s: %v", b, err)
		}
	}
	if out, err := run(t, dist(t, good()), prefix); err == nil || !strings.Contains(out, "not an empty directory") {
		t.Errorf("a used prefix: %v\n%s", err, out)
	}
	set := func(i int, change func(*fake)) []fake {
		fs := good()
		change(&fs[i])
		return fs
	}
	for name, c := range map[string]struct {
		fakes []fake
		extra []string
		want  string
	}{
		"schema sets differ":   {set(2, func(f *fake) { f.schemaSet = "sha256:" + strings.Repeat("cd", 32) }), nil, "reports schema set"},
		"commits differ":       {set(1, func(f *fake) { f.commit = "c2c2c2c" }), nil, "reports commit"},
		"a commit not in hex":  {set(0, func(f *fake) { f.commit = "unknown" }), nil, "reports commit"},
		"a short schema set":   {set(0, func(f *fake) { f.schemaSet = "sha256:abcd" }), nil, "reports schema set"},
		"an empty --expect":    {good(), []string{"--expect", ""}, "is not a sha256"},
		"a malformed --expect": {good(), []string{"--expect", "deadbeef"}, "is not a sha256"},
		"another platform":     {set(1, func(f *fake) { f.platform = "linux/riscv64" }), nil, "reports platform"},
		"a malformed set":      {set(0, func(f *fake) { f.schemaSet = "sha1:00" }), nil, "reports schema set"},
		"no commit":            {set(0, func(f *fake) { f.commit = "" }), nil, "reports commit"},
		"a missing binary":     {good()[:2], nil, "is missing"},
		"another expected":     {good(), []string{"--expect", "sha256:" + strings.Repeat("ef", 32)}, "expected"},
	} {
		if out, err := run(t, dist(t, c.fakes), filepath.Join(t.TempDir(), "p"), c.extra...); err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
	// adc answering as adcd.
	d := dist(t, good())
	adc := filepath.Join(d, "linux-"+runtime.GOARCH, "adc")
	wrong := strings.ReplaceAll(mustRead(t, adc), `"binary":"adc"`, `"binary":"adcd"`)
	if err := os.WriteFile(adc, []byte(wrong), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, d, filepath.Join(t.TempDir(), "p")); err == nil || !strings.Contains(out, "reports binary") {
		t.Errorf("a misnamed binary: %v\n%s", err, out)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
