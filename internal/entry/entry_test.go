// SPDX-License-Identifier: Apache-2.0

package entry

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMainExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no command", nil, ExitCouldNotRun},
		{"version", []string{"version"}, ExitOK},
		{"version json", []string{"version", "--json"}, ExitOK},
		{"help", []string{"--help"}, ExitOK},
		{"unknown command", []string{"bogus"}, ExitUsage},
		{"unknown flag", []string{"version", "--yaml"}, ExitUsage},
		{"dev without a subcommand", []string{"dev"}, ExitUsage},
		{"vocab check with an unknown flag", []string{"dev", "vocab", "check", "--yaml"}, ExitUsage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if got := Main("adc", c.args, &out, &errOut); got != c.want {
				t.Errorf("exit %d, want %d (stderr %q)", got, c.want, errOut.String())
			}
			if c.want != ExitOK && errOut.Len() == 0 {
				t.Errorf("non-zero exit with empty stderr")
			}
		})
	}
}

func TestVersionJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Main("adcd", []string{"version", "--json"}, &out, &errOut); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var got map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v: %q", err, out.String())
	}
	for _, k := range []string{"binary", "version", "commit", "schema_set", "go_version", "platform"} {
		if got[k] == "" {
			t.Errorf("field %q missing or empty in %v", k, got)
		}
	}
	if got["binary"] != "adcd" {
		t.Errorf("binary = %q, want adcd", got["binary"])
	}
}

// adc dev vocab check end to end on a synthetic module: clean exits 0 with a
// JSON report, an unchecked reason constructor exits 1 (refused) and names its
// position.
func TestVocabCheckCommand(t *testing.T) {
	module := func(t *testing.T, use string) string {
		t.Helper()
		dir := t.TempDir()
		files := map[string]string{
			"go.mod":                "module example.com/x\n\ngo 1.22\n",
			"gen/go/types/types.go": "package types\n\ntype ReasonCode struct{ code string }\n\nfunc ReasonCodeOf(c string) ReasonCode { return ReasonCode{c} }\n",
			"vocab/vocab.go":        "package vocab\n\nimport \"example.com/x/gen/go/types\"\n\ntype Reason = types.ReasonCode\n",
			"vocab/vocab.gen.go":    "package vocab\n\nimport \"example.com/x/gen/go/types\"\n\nvar ReasonStaleAttempt = types.ReasonCodeOf(\"stale_attempt\")\n",
			"use/use.go":            "package use\n\nimport (\n\t\"example.com/x/gen/go/types\"\n\t\"example.com/x/vocab\"\n)\n\nvar _ types.ReasonCode\n\nvar R vocab.Reason = " + use + "\n",
		}
		for name, src := range files {
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	var out, errOut bytes.Buffer
	clean := module(t, "vocab.ReasonStaleAttempt")
	if code := Main("adc", []string{"dev", "vocab", "check", "--root", clean, "--json"}, &out, &errOut); code != ExitOK {
		t.Fatalf("clean module: exit %d: %s", code, errOut.String())
	}
	var report struct {
		Reasons  int   `json:"reasons"`
		Findings []any `json:"findings"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.Reasons == 0 || len(report.Findings) != 0 {
		t.Fatalf("clean report %s (%v)", out.String(), err)
	}
	out.Reset()
	raw := module(t, `types.ReasonCodeOf("typo_reason")`)
	if code := Main("adc", []string{"dev", "vocab", "check", "--root", raw}, &out, &errOut); code != ExitRefused {
		t.Fatalf("raw literal: exit %d, want %d: %s", code, ExitRefused, errOut.String())
	}
	if !strings.Contains(out.String(), "use/use.go") || !strings.Contains(out.String(), "1 finding") {
		t.Fatalf("raw literal report does not name it: %s", out.String())
	}
	if code := Main("adcd", []string{"dev", "vocab", "check"}, &out, &errOut); code != ExitUsage {
		t.Fatalf("adcd dev: exit %d, want usage", code)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// Astra #24: could_not_run in JSON mode still prints one result document,
// and a report that cannot be written is could_not_run, never a pass.
func TestVocabCheckFailureOutput(t *testing.T) {
	var out, errOut bytes.Buffer
	missing := filepath.Join(t.TempDir(), "nowhere")
	if code := Main("adc", []string{"dev", "vocab", "check", "--root", missing, "--json"}, &out, &errOut); code != ExitCouldNotRun {
		t.Fatalf("missing root: exit %d", code)
	}
	var doc struct{ Outcome, Reason, Detail string }
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc.Outcome != "could_not_run" || doc.Reason != "check_unloadable" || doc.Detail == "" {
		t.Fatalf("JSON error document %q (%v)", out.String(), err)
	}
	if errOut.Len() == 0 {
		t.Fatal("no diagnostic on stderr")
	}
	errOut.Reset()
	if code := Main("adc", []string{"dev", "vocab", "check", "--root", "../.."}, failingWriter{}, &errOut); code != ExitCouldNotRun {
		t.Fatalf("unwritable report: exit %d, want %d", code, ExitCouldNotRun)
	}
}
