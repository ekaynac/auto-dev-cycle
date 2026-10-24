// SPDX-License-Identifier: Apache-2.0

package gate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/results"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	goMini     = "../../fixtures/m1/stub/go-mini" // the m1 records: baseline and acceptance
	goMiniT0   = "../../fixtures/t0/go-mini"      // the project: repo/ and tasks/
	goMiniRepo = goMiniT0 + "/repo"
	// pinnedARM64 is the platform image this repository's pinned prepared
	// Go image (…@sha256:4e8cf51b…, built with BASE_REF=0d3e81d from main
	// 8adbed7a, #226 entry 194) resolves to on linux/arm64, recorded apart
	// from any baseline (podman image inspect; Opus #300 L1).
	pinnedARM64 = "f80f244169466cb58bae92b54aaa4e295ce2f35f3642f85a8a858a73c19dee24"
	// goMiniARM64 is the platform image go-mini's own prepared image
	// (…-go-mini@sha256:66b8cd3a…, #226 entries 97 and 194, built from main 8adbed7a) resolves to on
	// linux/arm64 (podman image inspect).
	goMiniARM64 = "1053f9d45926bd7d2e51f54235b36bff4eb27532007fa71a28f8cb04ec15cbf5"
)

// recorded: a record's environment names the pin and go-mini's arm64
// platform image.
func recorded(image, id, pin string) bool { return image == pin && id == goMiniARM64 }

// TestGateGoMiniRecordStale (Opus #330 L1): a record still naming the old
// pin, or the old platform image, is not go-mini's.
func TestGateGoMiniRecordStale(t *testing.T) {
	const pin = "ghcr.io/ekaynac/adc-prepared-go-mini@sha256:66b8cd3a5aaee0cc9d6007e226f37e07d258b82edc49d91891048ae242268a31"
	old := "ghcr.io/ekaynac/adc-prepared-go-mini@sha256:f5b0ddd7d54c12317df029a4d296976950208b382c84e06285174efc30cddb95" // go-mini's pin before #226 entry 194
	if !recorded(pin, goMiniARM64, pin) || recorded(pin, pinnedARM64, pin) || recorded(old, goMiniARM64, pin) ||
		recorded(pin, "9c99adda7c9920e664db6eaec57d17c3e07acd7431184549c0455624d398d910", pin) { // the old pin's platform image
		t.Error("the record's environment check")
	}
}

// goMiniFiles are go-mini's repository files, by repo path.
func goMiniFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(goMiniRepo, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(goMiniRepo, p)
		files[filepath.ToSlash(rel)] = string(b)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

// TestGateGoMini (#226 entries 94, 97): go-mini's recorded baseline,
// measured by Measure in go-mini's own pinned image, binds its profile's
// command, that pin, the platform image it resolves to and the tree of
// T0's repo/ as committed, and so does the record's environment (Opus #330
// L1); the measured run, judged against it, is clean; the acceptance
// record names the stub task's committed hidden test and reference
// change; and the gate measures that tree green here too, with this
// host's go.
func TestGateGoMini(t *testing.T) {
	var b Baseline
	var p struct {
		Gate      types.ProjectProfileGate
		Toolchain struct{ Images map[string]string }
	}
	var rec struct {
		BaseTree    string `json:"base_tree"`
		Environment struct {
			Image   string `json:"image"`
			ImageID string `json:"image_id"`
		}
		Result struct {
			TestsPassed int `json:"tests_passed"`
		}
		Acceptance struct {
			TaskDir         string                          `json:"task_dir"`
			HiddenTests     []struct{ Path, Digest string } `json:"hidden_tests"`
			ReferenceDigest string                          `json:"reference_digest"`
		}
	}
	for file, into := range map[string]any{goMini + "/baseline-report.json": &b, goMini + "/baseline.json": &rec, "../../fixtures/m1/profiles/go-mini.json": &p} {
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
	in := In{Image: sandbox.Image{Pinned: pin, ID: goMiniARM64, OS: "linux", Arch: "arm64"}, Gate: p.Gate, Env: "ci", Base: f.tree, Baseline: &b}
	if err := in.check(); err != nil || pin == "" || b.Image != pin || b.Base != f.tree || rec.BaseTree != f.tree || !recorded(rec.Environment.Image, rec.Environment.ImageID, pin) || len(b.Results) != rec.Result.TestsPassed || !b.Green() {
		t.Fatalf("go-mini's baseline is not bound to its gate, image and tree: %v (%s, the profile pins %s; base %s, the record %s, the tree %s; %d results)",
			err, b.Image, pin, b.Base, rec.BaseTree, f.tree, len(b.Results))
	}
	var v Verdict
	if v.Results, err = results.ParseGoTestJSON(strings.NewReader(string(run)), nil, &b.Report); err != nil {
		t.Fatal(err)
	}
	if v.judge(in, "example.com/gomini", true); !is(v, vocab.Empty, vocab.ReasonCompletedClean) {
		t.Errorf("go-mini's base against its baseline: %v", v)
	}
	// The acceptance record is the committed task's (Opus #300 H1, L4).
	digest := func(path string) string {
		raw, err := os.ReadFile(filepath.Join("../..", rec.Acceptance.TaskDir, path))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	if len(rec.Acceptance.HiddenTests) == 0 || digest("reference.patch") != rec.Acceptance.ReferenceDigest {
		t.Errorf("the acceptance record's reference change: %+v", rec.Acceptance)
	}
	for _, h := range rec.Acceptance.HiddenTests {
		if digest(filepath.Join("hidden", h.Path)) != h.Digest {
			t.Errorf("the hidden test %s is not the recorded one", h.Path)
		}
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
	// The fixture's one-minute run limit cut this real go test (cold caches)
	// on a loaded runner, and a cut stream is unreadable (#226 entry 102):
	// the row checks the results, not the time, so its limit is generous.
	here.Limits.Timeout = 10 * time.Minute
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
