// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

// `adc dev scan` (#226 entry 206): secrets from a private file or stdin,
// never an argument; findings as quoted paths, every form redacted; exit 0
// clean, 1 found, 2 a search that did not complete, with one fixed line
// that names no path and no secret (Astra #443 r2 High 2).
func TestDevScan(t *testing.T) {
	const secret = "sk-filter-PROBE-0123456789abcdef"
	run := func(in string, args ...string) (string, string, int) {
		t.Helper()
		old := stdin
		stdin = strings.NewReader(in)
		defer func() { stdin = old }()
		var out, errs bytes.Buffer
		code := Main(context.Background(), append([]string{"dev", "scan"}, args...), &out, &errs, nil)
		return out.String(), errs.String(), code
	}
	dir := t.TempDir()
	put := func(p, content string, mode os.FileMode) string {
		t.Helper()
		p = filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tree := filepath.Join(dir, "tree")
	own := put("tree/home/token", secret+"\n", 0o600)
	put("tree/out/clean", "nothing\n", 0o600)
	if out, errs, code := run(secret+"\n", "--secrets", "-", "--own", own, tree); code != entry.ExitOK || out != "" || errs != "" {
		t.Errorf("clean, OWN exempt: exit %d %q %q", code, out, errs)
	}
	private := put("secrets", secret+"\n", 0o600)
	if _, _, code := run("", "--secrets", private, "--own", own, tree); code != entry.ExitOK {
		t.Errorf("the secrets from a private file: exit %d", code)
	}
	// A leaked copy named OWN's name and a newline: a finding, its name quoted.
	put("tree/home/token\n", secret, 0o600)
	if out, _, code := run(secret, "--secrets", "-", "--own", own, tree); code != entry.ExitRefused || out != `"`+own+`\n"`+"\n" {
		t.Errorf("a copy named OWN and a newline: exit %d %q", code, out)
	}
	if out, _, code := run(secret, "--secrets", "-", "--own", own, "--null", tree); code != entry.ExitRefused || out != own+"\n\x00" {
		t.Errorf("--null: exit %d %q", code, out)
	}
	if err := os.Remove(own + "\n"); err != nil {
		t.Fatal(err)
	}
	// A finding whose path holds the secret is printed redacted.
	put("tree/out/"+secret, secret, 0o600)
	if out, _, code := run(secret, "--secrets", "-", "--own", own, tree); code != entry.ExitRefused || strings.Contains(out, secret) || !strings.Contains(out, "out/[redacted]") {
		t.Errorf("a path holding the secret: exit %d %q", code, out)
	}
	if err := os.Remove(filepath.Join(tree, "out", secret)); err != nil {
		t.Fatal(err)
	}
	// An unreadable file whose name is the secret: incomplete (2), one fixed line, no path, no secret.
	if os.Getuid() != 0 {
		put("tree/out/"+secret, "x", 0o000)
		out, errs, code := run(secret, "--secrets", "-", "--own", own, tree)
		if code != entry.ExitCouldNotRun || out != "" || errs != "adc dev scan: could_not_run: the search did not complete\n" {
			t.Errorf("an unreadable file named the secret: exit %d %q %q", code, out, errs)
		}
		if err := os.Remove(filepath.Join(tree, "out", secret)); err != nil {
			t.Fatal(err)
		}
	}
	// The secrets' file must be private; no secret, no search.
	loose := put("loose", secret+"\n", 0o644)
	for name, c := range map[string]struct {
		in   string
		args []string
		code int
	}{
		"a file others read":   {"", []string{"--secrets", loose, tree}, entry.ExitCouldNotRun},
		"no secret":            {"\n", []string{"--secrets", "-", tree}, entry.ExitCouldNotRun},
		"a short secret":       {"short\n", []string{"--secrets", "-", tree}, entry.ExitCouldNotRun},
		"no root":              {secret, []string{"--secrets", "-"}, entry.ExitUsage},
		"nothing to search":    {"", []string{tree}, entry.ExitUsage},
		"stdin twice":          {"", []string{"--secrets", "-", "--canary", "-", tree}, entry.ExitUsage},
		"a name with a slash":  {"", []string{"--name", "a/b", tree}, entry.ExitUsage},
		"a secret as argument": {"", []string{"--secrets", secret, tree}, entry.ExitCouldNotRun},
	} {
		out, errs, code := run(c.in, c.args...)
		if code != c.code || strings.Contains(out+errs, secret) {
			t.Errorf("%s: exit %d %q %q", name, code, out, errs)
		}
	}
	// A name search, and the forms for the driver's redaction.
	put("beside/canary-x", "", 0o600)
	if out, _, code := run("", "--name", "canary-x", "--depth", "1", filepath.Join(dir, "beside")); code != entry.ExitRefused || !strings.Contains(out, "beside/canary-x") {
		t.Errorf("a name at depth 1: exit %d %q", code, out)
	}
	if out, _, code := run(secret+"\n", "--print-forms", "--secrets", "-"); code != entry.ExitOK || !strings.Contains(out, "b24f9f8a5b5eafe3d138113ed35db7e39ebbf3d69b71d79f\n") || strings.Count(out, "\n") != 6 {
		t.Errorf("--print-forms: exit %d %q", code, out)
	}
	// The canary, JSON-escaped in a captured body.
	put("caps/body", `{"messages":[{"content":"x\n\tconst q7vCanary = \"ADC-\u003cq7v\u003e-0123456789abcdef\""}]}`, 0o600)
	if out, _, code := run("\tconst q7vCanary = \"ADC-<q7v>-0123456789abcdef\"\n", "--canary", "-", filepath.Join(dir, "caps")); code != entry.ExitRefused || !strings.Contains(out, "caps/body") {
		t.Errorf("the canary in a body: exit %d %q", code, out)
	}
	// Opus #443 r3 O2: a secret's CR is its own byte: its base64 is of the whole, the CR included.
	crSecret := "sk-filter-PROBE-0123456789abcdef\r"
	if out, _, code := run(crSecret+"\n", "--print-forms", "--secrets", "-"); code != entry.ExitOK ||
		!strings.Contains(out, base64.StdEncoding.EncodeToString([]byte(crSecret))+"\n") {
		t.Errorf("a secret ending in CR: exit %d %q", code, out)
	}
	// Opus #443 r3 O1: the search ends with adc's context (its first signal), the fixed line.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	old := stdin
	stdin = strings.NewReader(secret)
	var out, errs bytes.Buffer
	code := Main(ctx, []string{"dev", "scan", "--secrets", "-", tree}, &out, &errs, nil)
	stdin = old
	if code != entry.ExitCouldNotRun || errs.String() != "adc dev scan: could_not_run: the search did not complete\n" {
		t.Errorf("an ended context: exit %d %q", code, errs.String())
	}
	// No link is followed: a link out of the roots is an incomplete search with --links-inside (the run's own
	// directory), neither followed nor reported without it (a sandbox root's layers).
	links := filepath.Join(dir, "links")
	if err := os.MkdirAll(links, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(links, "host")); err != nil {
		t.Fatal(err)
	}
	if _, errs, code := run(secret, "--secrets", "-", "--links-inside", links); code != entry.ExitCouldNotRun || errs != "adc dev scan: could_not_run: the search did not complete\n" {
		t.Errorf("a link out, --links-inside: exit %d %q", code, errs)
	}
	if _, _, code := run(secret, "--secrets", "-", links); code != entry.ExitOK {
		t.Errorf("a link out, neither followed nor reported: exit %d", code)
	}
	// The canary's forms for the driver (--print-forms --canary); --own-digests exempts a copy of an acceptance by its bytes.
	canary := put("canary", "\tconst q7vCanary = \"ADC-<q7v>-0123456789abcdef\"\n", 0o600)
	if out, _, code := run("", "--print-forms", "--canary", canary); code != entry.ExitOK || !strings.Contains(out, `ADC-\u003cq7v\u003e`) {
		t.Errorf("--print-forms --canary: exit %d %q", code, out)
	}
	accept := "package x\n\n\tconst q7vCanary = \"ADC-<q7v>-0123456789abcdef\"\n"
	put("acc/copy", accept, 0o600)
	sum := sha256.Sum256([]byte(accept))
	digests := put("digests", "sha256:"+hex.EncodeToString(sum[:])+"\n", 0o644)
	if _, _, code := run("", "--canary", canary, "--own-digests", digests, filepath.Join(dir, "acc")); code != entry.ExitOK {
		t.Errorf("an acceptance copy, by its digest: exit %d", code)
	}
	put("acc2/copy", accept, 0o600)
	put("acc2/log", "x "+accept, 0o600)
	if out, _, code := run("", "--canary", canary, "--own-digests", digests, filepath.Join(dir, "acc2")); code != entry.ExitRefused ||
		!strings.Contains(out, "acc2/log") || strings.Contains(out, "acc2/copy") {
		t.Errorf("only the acceptance's own bytes exempt: exit %d %q", code, out)
	}
	if _, _, code := run("", "--canary", canary, filepath.Join(dir, "acc")); code != entry.ExitRefused {
		t.Errorf("an acceptance copy, no digests: exit %d", code)
	}
	if _, _, code := run("", "--canary", canary, "--own-digests", canary, filepath.Join(dir, "acc")); code != entry.ExitCouldNotRun {
		t.Errorf("digests that are none: exit %d", code)
	}
}
