// SPDX-License-Identifier: Apache-2.0

package gate

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/results"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const goMini = "../../fixtures/m1/go-mini"

// goMiniFiles are go-mini's repository files, by repo path.
func goMiniFiles(t *testing.T) map[string]string {
	t.Helper()
	files, src := map[string]string{}, filepath.Join(goMini, "repo")
	if err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(src, p)
		files[filepath.ToSlash(rel)] = string(b)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

// TestGateGoMini (#226 entry 94): go-mini's recorded baseline, measured by
// Measure in the pinned image, binds its profile's command, that pin and
// the tree of fixtures/m1/go-mini/repo as committed; the measured run,
// judged against it, is clean; and the gate measures that tree green here
// too, with this host's go.
func TestGateGoMini(t *testing.T) {
	var b Baseline
	var p struct {
		Gate      types.ProjectProfileGate
		Toolchain struct{ Images map[string]string }
	}
	for file, into := range map[string]any{goMini + "/baseline-report.json": &b, "../../fixtures/m1/profiles/go-mini.json": &p} {
		raw, err := os.ReadFile(file)
		if err == nil {
			err = json.Unmarshal(raw, into)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	run, err := os.ReadFile(goMini + "/baseline-stream.json")
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, goMiniFiles(t), p.Gate.Command...)
	pin := p.Toolchain.Images["go"]
	in := In{Image: sandbox.Image{Pinned: pin, ID: b.ImageID, OS: b.OS, Arch: b.Arch}, Gate: p.Gate, Env: "ci", Base: f.tree, Baseline: &b}
	if err := in.check(); err != nil || pin == "" || b.Image != pin || b.Base != f.tree || len(b.Results) != 4 || !b.Green() {
		t.Fatalf("go-mini's baseline is not bound to its gate and tree: %v (%s, the profile pins %s; base %s, the tree %s; %d results)",
			err, b.Image, pin, b.Base, f.tree, len(b.Results))
	}
	var v Verdict
	if v.Results, err = results.ParseGoTestJSON(strings.NewReader(string(run)), nil, &b.Report); err != nil {
		t.Fatal(err)
	}
	if v.judge(in, "example.com/go-mini", true); !is(v, vocab.Empty, vocab.ReasonCompletedClean) {
		t.Errorf("go-mini's base against its baseline: %v", v)
	}
	// Measured here, in place, with this host's go, offline: green, and its
	// results the recorded baseline's.
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on PATH")
	}
	cache := t.TempDir()
	here := f.in
	here.Gate = p.Gate
	here.Gate.Environments = slices.Clone(p.Gate.Environments)
	here.Gate.Environments[0].Facts.Runtime.Os, here.Gate.Environments[0].Facts.Runtime.Arch = runtime.GOOS, runtime.GOARCH
	here.Host.Runner = &local{env: append(testguard.HomeEnv(t), "PATH="+filepath.Dir(gobin)+":/usr/bin:/bin", "GOCACHE="+filepath.Join(cache, "build"),
		"GOPATH="+filepath.Join(cache, "path"), "GOTOOLCHAIN=local", "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off")}
	got, mv, err := Measure(context.Background(), here)
	names := func(r results.Report) (out []string) {
		for _, x := range r.Results {
			out = append(out, x.Package+"."+x.Name+"="+string(x.Outcome))
		}
		slices.Sort(out)
		return out
	}
	if err != nil || !is(mv, vocab.Empty, vocab.ReasonCompletedClean) || got.Base != f.tree || !slices.Equal(names(got.Report), names(b.Report)) {
		t.Errorf("go-mini measured here: %v %v %s, results %v", err, mv.Outcome, mv.Why, names(got.Report))
	}
}
