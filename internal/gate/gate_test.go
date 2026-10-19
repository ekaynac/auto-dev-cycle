// SPDX-License-Identifier: Apache-2.0

package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/results"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

var gitEnv = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "PATH=/usr/bin:/bin",
	"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}

func plain(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, append(testguard.HomeEnv(t), gitEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// local runs a gate container's command in place: the attempt's
// workspace as its cwd, the container's mount paths mapped to the
// attempt's directories. It is the sandbox's Runner boundary.
type local struct{}

func (l *local) Run(ctx context.Context, _ string, args, _ []string) ([]byte, error) {
	if !slices.Contains(args, "run") { // after podman's global flags
		return nil, nil // rm, unshare
	}
	var ws, res string
	i := slices.Index(args, testImage.ID)
	for _, a := range args[:i] {
		if m, ok := strings.CutPrefix(a, "--mount=type=bind,src="); ok {
			src, dst, _ := strings.Cut(m, ",dst=")
			if dst == sandbox.WorkspaceDir {
				ws = src
			} else if dst == sandbox.ResultsDir {
				res = src
			}
		}
	}
	mapped := strings.NewReplacer(sandbox.WorkspaceDir, ws, sandbox.ResultsDir, res)
	argv := make([]string, 0, len(args)-i-1)
	for _, a := range args[i+1:] {
		argv = append(argv, mapped.Replace(a))
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = ws, []string{"PATH=/usr/bin:/bin"}
	return cmd.Output()
}

var testImage = sandbox.Image{Pinned: "localhost/prepared@sha256:" + strings.Repeat("a", 64), ID: strings.Repeat("b", 64), OS: runtime.GOOS, Arch: runtime.GOARCH}

const module = "example.com/m"

// events is a go test -json stream: each test "pkg.Test=outcome", every
// package passing unless one of its tests failed.
func events(tests ...string) string {
	var b strings.Builder
	failed := map[string]bool{}
	var pkgs []string
	for _, x := range tests {
		pt, o, _ := strings.Cut(x, "=")
		pkg, test, _ := strings.Cut(pt, ".")
		pkg = module + "/" + pkg
		if !slices.Contains(pkgs, pkg) {
			pkgs = append(pkgs, pkg)
			fmt.Fprintf(&b, `{"Action":"start","Package":%q}`+"\n", pkg)
		}
		fmt.Fprintf(&b, `{"Action":"run","Package":%q,"Test":%q}`+"\n", pkg, test)
		fmt.Fprintf(&b, `{"Action":"output","Package":%q,"Test":%q,"Output":"CANARY-FAILURE-TEXT %s\n"}`+"\n", pkg, test, test)
		fmt.Fprintf(&b, `{"Action":%q,"Package":%q,"Test":%q}`+"\n", o, pkg, test)
		failed[pkg] = failed[pkg] || o == "fail"
	}
	for _, p := range pkgs {
		fmt.Fprintf(&b, `{"Action":%q,"Package":%q}`+"\n", map[bool]string{true: "fail", false: "pass"}[failed[p]], p)
	}
	return b.String()
}

type fixture struct {
	in   In
	tree string // the base tree's id
	head string // its commit
}

// newFixture is a repository whose tree holds files, a sandbox host on a
// tmpfs (runs/ on its own filesystem) running commands in place, and a
// gate on the base report base.
func newFixture(t *testing.T, files map[string]string, command ...string) fixture {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	plain(t, filepath.Dir(src), "init", "--quiet", "--initial-branch=main", src)
	files["go.mod"] = "module " + module + "\n\ngo 1.24\n"
	for p, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(src, p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, p), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plain(t, src, "add", "--all")
	plain(t, src, "commit", "--quiet", "-m", "base")
	repo, err := sandbox.Host{Root: t.TempDir()}.Clone(context.Background(), src, "refs/heads/main", filepath.Join(t.TempDir(), "ctl"))
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/dev/shm", "g")
	if err != nil {
		t.Fatalf("the sandbox root's runs/ needs a tmpfs of its own (/dev/shm): %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	base, err := results.ParseGoTestJSON(strings.NewReader(events("a.TestA=pass", "b.TestB=pass")), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	gate := types.ProjectProfileGate{Command: command, ResultAdapter: results.GoTestJSON, OwnershipRules: "default",
		Environments: []types.ProjectProfileGateEnvironmentsItem{{Name: "ci", Owner: "operator",
			Facts: types.ProjectProfileGateEnvironmentsItemFacts{Runtime: types.ProjectProfileGateEnvironmentsItemFactsRuntime{Os: runtime.GOOS, Arch: runtime.GOARCH}}}}}
	return fixture{tree: plain(t, src, "rev-parse", "HEAD^{tree}"), head: plain(t, src, "rev-parse", "HEAD"), in: In{
		Host:  sandbox.Host{Podman: "/usr/bin/podman", Root: root, Home: "/home/adc-gate-test", Runner: &local{}},
		Image: testImage, Repo: repo, Tree: plain(t, src, "rev-parse", "HEAD^{tree}"), Gate: gate, Env: "ci",
		Baseline: &Baseline{Command: command, Image: testImage.Pinned, Report: base},
		Limits:   sandbox.Limits{Memory: 1 << 30, CPUs: "1", PIDs: 100, Timeout: time.Minute, Disk: 64 << 20}}}
}

func run(t *testing.T, in In) Verdict {
	t.Helper()
	v, err := Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return v
}

func is(v Verdict, o vocab.Outcome, r vocab.Reason) bool { return v.Outcome == o && v.Reason == r }

var stream = map[string]string{
	"clean.json":   events("a.TestA=pass", "b.TestB=pass"),
	"failing.json": events("a.TestA=fail", "b.TestB=pass"),
	"missing.json": events("a.TestA=pass"), // b never reported
	"touched.json": events("b.TestB=pass"), // a never reported
}

// TestGateVerdict: the verdict comes from the derived results file alone,
// judged against the baseline under the default ownership rules.
func TestGateVerdict(t *testing.T) {
	files := map[string]string{"a/a.go": "package a\n", "b/b.go": "package b\n"}
	for k, s := range stream {
		files[k] = s
	}
	for name, c := range map[string]struct {
		command []string
		changed []string
		want    string // in the verdict
	}{
		"clean, on stdout":                   {[]string{"cat", "clean.json"}, nil, "empty/completed_clean"},
		"clean, at {results_path}":           {[]string{"cp", "clean.json", "{results_path}"}, nil, "empty/completed_clean"},
		"a failing test":                     {[]string{"cat", "failing.json"}, nil, "found/gate_test_failed"},
		"lying runner: failing, exit 0":      {[]string{"sh", "-c", `cp failing.json "$0"; echo "5 passed"; exit 0`, "{results_path}"}, nil, "found/gate_test_failed"},
		"results never written":              {[]string{"sh", "-c", `echo "5 passed"; exit 0`, "{results_path}"}, nil, "runner_fault: the run never wrote"},
		"results unreadable":                 {[]string{"echo", "ok"}, nil, "runner_fault: the run's results are not"},
		"a base test missing, untouched":     {[]string{"cat", "missing.json"}, []string{"a/a.go"}, "runner_fault: 2 results missing"},
		"a base test missing in its package": {[]string{"cat", "touched.json"}, []string{"a/a.go"}, "found/gate_test_failed"},
	} {
		f := newFixture(t, files, c.command...)
		f.in.Changed = c.changed
		if v := run(t, f.in); !strings.Contains(v.String(), c.want) || v.Tree != f.tree {
			t.Errorf("%s: %v (tree %s), want %s", name, v, v.Tree, c.want)
		}
	}
	// An expected (acceptance) test, overlaid on the workspace only, that
	// never ran is the candidate's, wherever it is.
	f := newFixture(t, files, "cat", "acc/stream.json")
	f.in.Overlay = map[string][]byte{"acc/stream.json": []byte(stream["clean.json"])}
	f.in.Expected = []results.Test{{Package: module + "/c", Name: "TestHidden"}}
	if v := run(t, f.in); !is(v, vocab.Found, vocab.ReasonGateTestFailed) {
		t.Errorf("a missing acceptance test: %v", v)
	}
	f.in.Overlay = map[string][]byte{"../escape": nil}
	if _, err := Run(context.Background(), f.in); !errors.Is(err, ErrSpec) {
		t.Errorf("an overlay outside the workspace: %v", err)
	}
}

// TestGateBaseline: a run without a baseline measures one, green only; a
// baseline of another command or image is refused, as is an image of
// another platform than the environment's (#226 entry 88).
func TestGateBaseline(t *testing.T) {
	f := newFixture(t, map[string]string{"clean.json": stream["clean.json"], "failing.json": stream["failing.json"]}, "cat", "clean.json")
	in := f.in
	in.Baseline = nil
	if v := run(t, in); !is(v, vocab.Empty, vocab.ReasonCompletedClean) || !v.Results.Green() {
		t.Errorf("a green base: %v", v)
	}
	in.Gate.Command = []string{"cat", "failing.json"}
	if v := run(t, in); !is(v, vocab.CouldNotRun, vocab.ReasonRunnerFault) {
		t.Errorf("a base that is not green: %v", v)
	}
	for name, mutate := range map[string]func(*In){
		"another command": func(in *In) { in.Baseline = &Baseline{Command: []string{"go", "test"}, Image: in.Image.Pinned} },
		"another image": func(in *In) {
			in.Baseline = &Baseline{Command: in.Gate.Command, Image: "localhost/other@sha256:" + strings.Repeat("c", 64)}
		},
		"another architecture": func(in *In) { in.Image.Arch = map[string]string{"arm64": "amd64"}[runtime.GOARCH] + "x" },
		"another environment":  func(in *In) { in.Env = "prod" },
		"another adapter":      func(in *In) { in.Gate.ResultAdapter = "junit_xml" },
	} {
		in := f.in
		mutate(&in)
		if _, err := Run(context.Background(), in); !errors.Is(err, ErrSpec) {
			t.Errorf("%s: %v, want ErrSpec", name, err)
		}
	}
	// The fixture baseline decodes as a Baseline (P32's encoding).
	b, err := os.ReadFile("../../fixtures/m1/issues/170/baseline-report.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture Baseline
	if err := json.Unmarshal(b, &fixture); err != nil || len(fixture.Results) != 72 || len(fixture.Command) == 0 || !fixture.Green() {
		t.Errorf("issue 170's baseline: %d results, command %q, %v", len(fixture.Results), fixture.Command, err)
	}
}

// TestGateCwd (#275): every Python process starts in the checkout root and
// changes directory only after collection; the runner/ counterexample, a
// worker started elsewhere and an early chdir invalidate the run.
func TestGateCwd(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("the cwd rows need python3")
	}
	files := map[string]string{"clean.json": stream["clean.json"], "conftest.py": "import sys\nsys.path.insert(0, 'lib')\n",
		"lib/feature.py": "FLAG = False\n", "runner/lib/feature.py": "FLAG = True\n", "runner/README": "runner\n"}
	py := `python3 -c 'import sys; sys.path.insert(0, "lib"); import feature'`
	for name, c := range map[string]struct {
		script string
		want   string
	}{
		"python at the root":            {py + ` && cat clean.json`, "empty/completed_clean"},
		"cd runner && pytest .. (#275)": {`cd runner && ` + py + ` && cat ../clean.json`, "the cwd contract"},
		"a worker started in runner":    {`python3 -c 'import subprocess; subprocess.run(["python3", "-c", "pass"], cwd="runner", check=True)' && cat clean.json`, "the cwd contract"},
		"a chdir before collection":     {`python3 -c 'import os; os.chdir("runner")' && cat clean.json`, "the cwd contract"},
	} {
		f := newFixture(t, files, "sh", "-c", c.script)
		if v := run(t, f.in); !strings.Contains(v.String(), c.want) {
			t.Errorf("%s: %v, want %s", name, v, c.want)
		}
	}
}

// TestGateTree: the receipt is about the tree the run checked out; a
// commit id given as the tree is refused, and a receipt of another tree,
// image or contract revision does not apply.
func TestGateTree(t *testing.T) {
	f := newFixture(t, map[string]string{"clean.json": stream["clean.json"]}, "cat", "clean.json")
	in := f.in
	in.Tree = f.head
	if _, err := Run(context.Background(), in); !errors.Is(err, ErrSpec) {
		t.Errorf("a commit id as the tree: %v, want ErrSpec", err)
	}
	blobs, rcpt, _, c, s := publish(t, f, "cat", "clean.json")
	now := time.Now()
	if err := receipt.Applies(rcpt, Requirement(c, f.tree, testImage, s), now); err != nil {
		t.Fatalf("the receipt of its own tree: %v", err)
	}
	other := testImage
	other.ID = strings.Repeat("d", 64)
	revised := s
	revised.Revision = "2"
	for name, req := range map[string]receipt.Requirement{
		"another tree":     Requirement(c, strings.Repeat("e", 40), testImage, s),
		"another image":    Requirement(c, f.tree, other, s),
		"another revision": Requirement(c, f.tree, testImage, revised),
	} {
		var refusal *receipt.Refusal
		if err := receipt.Applies(rcpt, req, now); !errors.As(err, &refusal) {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
	if err := receipt.Check(c, rcpt, blobs); err != nil {
		t.Errorf("the recorded receipt is not the derived one: %v", err)
	}
}

// publish runs f's gate with command and publishes its result.
func publish(t *testing.T, f fixture, command ...string) (*blobstore.Store, types.Receipt, types.Result, types.InstrumentContract, Subject) {
	t.Helper()
	f.in.Gate.Command = command
	f.in.Baseline.Command = command
	v := run(t, f.in)
	blobs, err := blobstore.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Contract(f.in.Gate, testImage)
	if err != nil {
		t.Fatal(err)
	}
	s := Subject{Project: "prj_01m3250vz2ezn81cgvbkcn17jd", Revision: "1"}
	res, err := Result(types.ComputationKey{Key: types.Digest("sha256:" + strings.Repeat("1", 64))}, v, c, testImage, s, "cmd_01m3250vz2ezn81cgvbkcn17jd", blobs, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var rcpt types.Receipt
	if err := json.Unmarshal(read(t, blobs, res.Value[0].ContentDigest), &rcpt); err != nil {
		t.Fatal(err)
	}
	return blobs, rcpt, res, c, s
}

func read(t *testing.T, blobs *blobstore.Store, d types.Digest) []byte {
	t.Helper()
	rc, err := blobs.Open(string(d))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestGateSanitised: a reviewer's context renders a gate result's value
// whole (graph.Context classes no gate result), so no blob it names holds
// the run's failure text; the raw results stay the receipt's restricted
// evidence. The value is the receipt and the D55 report.
func TestGateSanitised(t *testing.T) {
	f := newFixture(t, map[string]string{"failing.json": stream["failing.json"], "a/a.go": "package a\n"}, "cat", "failing.json")
	blobs, rcpt, res, c, _ := publish(t, f, "cat", "failing.json")
	if res.Outcome != types.Outcome(vocab.Found) || res.Reason != vocab.ReasonGateTestFailed || !res.Completed || len(res.Value) != 2 {
		t.Fatalf("result %s/%s, %d refs", res.Outcome, res.Reason, len(res.Value))
	}
	for _, ref := range res.Value {
		if b := read(t, blobs, ref.ContentDigest); strings.Contains(string(b), "CANARY-FAILURE-TEXT") {
			t.Errorf("the result's %s ref holds failure text", ref.Type)
		}
	}
	if strings.Contains(res.Detail, "CANARY") {
		t.Errorf("the result's detail holds failure text")
	}
	ev := rcpt.Predicates[0].Evidence[0]
	if c.ArtifactClasses[ev.Artifact] != types.InstrumentContractArtifactClassesValueRestricted || !strings.Contains(string(read(t, blobs, ev.Blob)), "CANARY-FAILURE-TEXT") {
		t.Errorf("the raw results are not kept as restricted evidence")
	}
	var report struct {
		Schema      string         `json:"schema"`
		ElapsedMS   *int64         `json:"elapsed_ms"`
		Blocking    []any          `json:"blocking"`
		ChargeLinks []types.CallID `json:"charge_links"`
	}
	if err := json.Unmarshal(read(t, blobs, res.Value[1].ContentDigest), &report); err != nil || report.Schema != reportSchema ||
		report.ElapsedMS == nil || len(report.Blocking) != 2 || report.ChargeLinks == nil {
		t.Errorf("the D55 report: %+v, %v", report, err)
	}
}
