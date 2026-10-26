// SPDX-License-Identifier: Apache-2.0

package referee

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The rows run Judge against a fake podman: a script that answers image
// exists and rm, and for run finds the workspace in its mount and plays a
// scenario's run. A run with no --network=none is the setup; one whose
// workspace holds the acceptance file is H, or C when the patch's marker is
// in it too; any other is B. Each run writes <fake>/<run>.xml to junit.xml
// (or prints <fake>/<run>.json), sleeps <fake>/<run>.sleep seconds, and
// exits <fake>/<run>.exit. Every call's arguments go to <fake>/calls.
const fakePodman = `#!/bin/sh
fake=%s
echo "$*" >> "$fake/calls"
[ "$1" = run ] || exit 0
net=1
for a in "$@"; do
  case "$a" in
    --network=none) net=0 ;;
    --mount=type=bind,src=*) ws=${a#--mount=type=bind,src=}; ws=${ws%%,dst=*} ;;
  esac
done
if [ $net = 1 ]; then touch "$ws/.setup-done"; exit 0; fi
run=B
if [ -f "$ws/accept/hidden.txt" ]; then run=H; grep -q fixed "$ws/marker" && run=C; fi
[ -f "$fake/$run.xml" ] && cp "$fake/$run.xml" "$ws/junit.xml"
[ -f "$fake/$run.json" ] && cat "$fake/$run.json"
[ -f "$fake/$run.sleep" ] && sleep "$(cat "$fake/$run.sleep")"
[ -f "$fake/$run.exit" ] && exit "$(cat "$fake/$run.exit")"
exit 0
`

type scenario struct {
	t    *testing.T
	dir  string // the item's directory
	fake string
	base string
	item map[string]any
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir, cmd.Env = dir, []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o700); err != nil {
		t.Fatal(err)
	}
}

// newScenario makes a repository (marker "base", a visible test file), the
// acceptance file, the fake podman and a junit_xml item.
func newScenario(t *testing.T) *scenario {
	s := &scenario{t: t, dir: t.TempDir(), fake: t.TempDir()}
	repo := filepath.Join(s.dir, "repo")
	write(t, filepath.Join(repo, "marker"), "base\n")
	write(t, filepath.Join(repo, "tests/visible.txt"), "visible\n")
	git(t, repo, "init", "-q")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "base")
	s.base = git(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(s.dir, "hidden.txt"), "SECRET-ACCEPTANCE-CONTENT\n")
	write(t, filepath.Join(s.fake, "podman"), strings.Replace(fakePodman, "%s", s.fake, 1))
	s.item = map[string]any{
		"repo": "repo", "base": s.base, "image": "example.invalid/img@sha256:" + strings.Repeat("a", 64),
		"setup": []string{"deps"}, "test_command": []string{"test"},
		"results":      map[string]any{"format": "junit_xml", "path": "junit.xml"},
		"acceptance":   []map[string]any{{"path": "accept/hidden.txt", "file": "hidden.txt"}},
		"hidden_tests": []string{"s > c > h1", "s > c > h2"}, "timeout_seconds": 30,
	}
	return s
}

// junit writes run's JUnit file: tests "s > c > NAME", each "pass",
// "fail", "error" or "skip".
func (s *scenario) junit(run string, tests map[string]string) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><testsuites><testsuite name="s">`)
	for name, o := range tests {
		b.WriteString(`<testcase classname="c" name="` + name + `">`)
		switch o {
		case "fail":
			b.WriteString(`<failure message="x"/>`)
		case "error":
			b.WriteString(`<error/>`)
		case "skip":
			b.WriteString(`<skipped/>`)
		}
		b.WriteString(`</testcase>`)
	}
	b.WriteString(`</testsuite></testsuites>`)
	write(s.t, filepath.Join(s.fake, run+".xml"), b.String())
}

// patch is a git diff against the base: the marker fixed, and extra.
func (s *scenario) patch(extra map[string]string) []byte {
	work := filepath.Join(s.t.TempDir(), "w")
	git(s.t, "/", "clone", "-q", filepath.Join(s.dir, "repo"), work)
	write(s.t, filepath.Join(work, "marker"), "fixed\n")
	for p, c := range extra {
		write(s.t, filepath.Join(work, p), c)
	}
	git(s.t, work, "add", "-A")
	return []byte(git(s.t, work, "diff", "--cached", "--binary", s.base) + "\n")
}

func (s *scenario) judge(patch []byte) Report {
	item, err := json.Marshal(s.item)
	if err != nil {
		s.t.Fatal(err)
	}
	return Judge(context.Background(), In{Podman: filepath.Join(s.fake, "podman"), Item: item, ItemDir: s.dir, Patch: patch})
}

func (s *scenario) green() {
	s.junit("B", map[string]string{"v1": "pass", "v2": "pass", "v3": "fail"})
	s.junit("H", map[string]string{"v1": "pass", "v2": "pass", "v3": "fail", "h1": "fail", "h2": "pass"})
	s.junit("C", map[string]string{"v1": "pass", "v2": "pass", "v3": "fail", "h1": "pass", "h2": "pass"})
}

func want(t *testing.T, r Report, v Verdict, reasons ...string) {
	t.Helper()
	if r.Verdict != v || strings.Join(r.Reasons, "\n") != strings.Join(reasons, "\n") {
		t.Fatalf("got %s %q, want %s %q", r.Verdict, r.Reasons, v, reasons)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "SECRET-ACCEPTANCE-CONTENT") {
		t.Fatalf("the report holds the acceptance's content: %s", b)
	}
}

// TestVerdicts: the hidden tests are h1, failing at base, and h2, which
// guards what the base already does (it passes at base with the acceptance).
func TestVerdicts(t *testing.T) {
	for name, row := range map[string]struct {
		b, h, c map[string]string
		v       Verdict
		reasons []string
	}{
		"accepted": {v: Accepted},
		"hidden missing": {c: map[string]string{"v1": "pass", "v2": "pass", "h2": "pass"}, v: Rejected,
			reasons: []string{"hidden test not reported in the candidate run: s > c > h1"}},
		"hidden failing": {c: map[string]string{"v1": "pass", "v2": "pass", "h1": "error", "h2": "pass"}, v: Rejected,
			reasons: []string{"hidden test failing in the candidate run: s > c > h1"}},
		"hidden skipped": {c: map[string]string{"v1": "pass", "v2": "pass", "h1": "skip", "h2": "pass"}, v: Rejected,
			reasons: []string{"hidden test skipped in the candidate run: s > c > h1"}},
		"hidden guard broken": {c: map[string]string{"v1": "pass", "v2": "pass", "h1": "pass", "h2": "fail"}, v: Rejected,
			reasons: []string{"hidden test failing in the candidate run: s > c > h2"}},
		"visible deleted": {c: map[string]string{"v1": "pass", "h1": "pass", "h2": "pass"}, v: Rejected,
			reasons: []string{"visible test not reported in the candidate run: s > c > v2"}},
		"visible newly failing": {c: map[string]string{"v1": "fail", "v2": "skip", "h1": "pass", "h2": "pass"}, v: Rejected,
			reasons: []string{"visible test failing in the candidate run: s > c > v1", "visible test skipped in the candidate run: s > c > v2"}},
		"acceptance passing at base": {h: map[string]string{"v1": "pass", "v2": "pass", "h1": "pass", "h2": "pass"}, v: InvalidItem,
			reasons: []string{"every hidden test passes at the base"}},
		"hidden test visible at base": {b: map[string]string{"v1": "pass", "v2": "pass", "h2": "pass"}, v: InvalidItem,
			reasons: []string{"a hidden test is reported without the acceptance: s > c > h2"}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newScenario(t)
			s.green()
			for run, tests := range map[string]map[string]string{"B": row.b, "H": row.h, "C": row.c} {
				if tests != nil {
					s.junit(run, tests)
				}
			}
			r := s.judge(s.patch(nil))
			want(t, r, row.v, row.reasons...)
			if row.v == Accepted && (r.Runs["candidate"].Passed != 4 || r.Runs["base"].Failed != 1 || r.Runs["setup"] == nil) {
				t.Fatalf("runs: %+v", r.Runs)
			}
		})
	}
}

func TestEmptyHiddenSetIsInvalid(t *testing.T) {
	s := newScenario(t)
	s.green()
	s.item["hidden_tests"] = []string{}
	want(t, s.judge(s.patch(nil)), InvalidItem, "the item does not fit schemas/testing/referee-item.json")
}

func TestGoTestJSON(t *testing.T) {
	ev := func(test, action string) string {
		return `{"Action":"run","Package":"p","Test":"` + test + `"}` + "\n" + `{"Action":"` + action + `","Package":"p","Test":"` + test + `"}` + "\n"
	}
	s := newScenario(t)
	s.item["results"] = map[string]any{"format": "go_test_json", "path": "stdout"}
	s.item["hidden_tests"] = []string{"p > TestH"}
	write(t, filepath.Join(s.fake, "B.json"), ev("TestV", "pass")+`{"Action":"pass","Package":"p"}`+"\n")
	write(t, filepath.Join(s.fake, "H.json"), `{"Action":"build-fail","ImportPath":"p [p.test]"}`+"\n"+`{"Action":"fail","Package":"p"}`+"\n")
	write(t, filepath.Join(s.fake, "H.exit"), "1")
	write(t, filepath.Join(s.fake, "C.json"), ev("TestV", "pass")+ev("TestH", "pass")+`{"Action":"pass","Package":"p"}`+"\n")
	want(t, s.judge(s.patch(nil)), Accepted)
	write(t, filepath.Join(s.fake, "C.json"), ev("TestV", "pass")+ev("TestH", "fail")+`{"Action":"fail","Package":"p"}`+"\n")
	write(t, filepath.Join(s.fake, "C.exit"), "1")
	want(t, s.judge(s.patch(nil)), Rejected, "hidden test failing in the candidate run: p > TestH")
}

// TestCandidateRunMustComplete: a timeout, a crash, or a non-zero exit with
// no failing test reported (here the base's failing v3 passes too) rejects.
func TestCandidateRunMustComplete(t *testing.T) {
	for name, row := range map[string]struct {
		file, content, reason string
	}{
		"timeout":                  {"C.sleep", "5", "the candidate run timed out"},
		"crash":                    {"C.exit", "137", "the candidate run did not complete: exit 137"},
		"non-zero with no failure": {"C.exit", "1", "the candidate run did not complete: exit 1"},
		"podman fault":             {"C.exit", "125", ""},
	} {
		t.Run(name, func(t *testing.T) {
			s := newScenario(t)
			s.green()
			s.junit("C", map[string]string{"v1": "pass", "v2": "pass", "v3": "pass", "h1": "pass", "h2": "pass"})
			s.item["timeout_seconds"] = 2
			write(t, filepath.Join(s.fake, row.file), row.content)
			r := s.judge(s.patch(nil))
			if row.reason == "" {
				want(t, r, CouldNotRun, "the candidate run did not start")
				return
			}
			want(t, r, Rejected, row.reason)
		})
	}
}

func TestPatchRefusals(t *testing.T) {
	symlink := "diff --git a/link b/link\nnew file mode 120000\n--- /dev/null\n+++ b/link\n@@ -0,0 +1 @@\n+/etc/passwd\n\\ No newline at end of file\n"
	for name, row := range map[string]struct {
		extra  map[string]string
		raw    string
		reason string
	}{
		"acceptance path":      {extra: map[string]string{"accept/hidden.txt": "mine\n"}, reason: "the patch is refused: an acceptance path: accept/hidden.txt"},
		"acceptance directory": {extra: map[string]string{"accept": "a file\n"}, reason: "the patch is refused: an acceptance path: accept"},
		"git control":          {extra: map[string]string{".gitattributes": "* filter=x\n"}, reason: "the patch is refused: a protected path: .gitattributes"},
		"symlink":              {raw: symlink, reason: "the patch is refused: a symlink: link"},
		"not applying":         {raw: "diff --git a/nope b/nope\n--- a/nope\n+++ b/nope\n@@ -1 +1 @@\n-x\n+y\n", reason: "the patch is refused: git does not apply the patch to the base: a path git refuses (.., .git, absolute) or a hunk that does not apply: "},
		"write into .git":      {raw: "diff --git a/.git/config b/.git/config\nnew file mode 100644\n--- /dev/null\n+++ b/.git/config\n@@ -0,0 +1 @@\n+x\n", reason: "the patch is refused: git does not apply the patch to the base: a path git refuses (.., .git, absolute) or a hunk that does not apply: "},
	} {
		t.Run(name, func(t *testing.T) {
			s := newScenario(t)
			s.green()
			p := []byte(row.raw)
			if row.raw == "" {
				p = s.patch(row.extra)
			}
			r := s.judge(p)
			want(t, r, Rejected, row.reason)
			if len(r.Runs) != 0 {
				t.Fatalf("a refused patch ran: %+v", r.Runs)
			}
		})
	}
}

func TestItemRefusals(t *testing.T) {
	for name, row := range map[string]struct {
		key    string
		value  any
		reason string
	}{
		"unsafe acceptance path": {"acceptance", []map[string]any{{"path": "../x", "file": "hidden.txt"}}, "an acceptance path is not a plain repository path, or is listed twice: ../x"},
		"git acceptance path":    {"acceptance", []map[string]any{{"path": ".git/hooks/x", "file": "hidden.txt"}}, "an acceptance path is not a plain repository path, or is listed twice: .git/hooks/x"},
		"missing acceptance":     {"acceptance", []map[string]any{{"path": "a", "file": "nope"}}, "an acceptance file cannot be read: a"},
		"unsafe results path":    {"results", map[string]any{"format": "junit_xml", "path": "/etc/x"}, "the results path is not a plain repository path"},
		"unknown base":           {"base", strings.Repeat("b", 40), "the base is not a commit of the item's repository"},
		"unpinned image":         {"image", "docker.io/library/python:3", "the item does not fit schemas/testing/referee-item.json"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newScenario(t)
			s.green()
			s.item[row.key] = row.value
			want(t, s.judge(nil), InvalidItem, row.reason)
		})
	}
}

// TestRunsAreConfined: the setup alone has the network; every run gets the
// sandbox's confinement, the item's image and its argv as the entrypoint.
func TestRunsAreConfined(t *testing.T) {
	s := newScenario(t)
	s.green()
	want(t, s.judge(s.patch(nil)), Accepted)
	b, err := os.ReadFile(filepath.Join(s.fake, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	var runs []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "run ") {
			runs = append(runs, l)
		}
	}
	if len(runs) != 4 || strings.Contains(runs[0], "--network=none") || !strings.Contains(runs[0], `--entrypoint=["deps"]`) {
		t.Fatalf("runs: %q", runs)
	}
	for _, r := range runs[1:] {
		for _, f := range []string{"--network=none", "--read-only", "--cap-drop=all", "--env-host=false", `--entrypoint=["test"] example.invalid/img@sha256:`} {
			if !strings.Contains(r, f) {
				t.Fatalf("a test run without %s: %s", f, r)
			}
		}
	}
}

func TestParseJUnit(t *testing.T) {
	x, err := parseJUnit([]byte(`<testsuite name="a"><testsuite name="b"><testcase name="t"/><testcase name="t"><failure/></testcase><testcase name="t"/></testsuite><testcase classname="k" name="u"><skipped/></testcase></testsuite>`))
	if err != nil || x.by["a > b > t"] != fail || x.by["a > k > u"] != skip || len(x.by) != 2 || !x.failures {
		t.Fatalf("%+v %v", x, err)
	}
	for _, bad := range []string{"", "<html/>", `<testsuites><testcase name="a"><testcase name="b"/></testcase></testsuites>`, "<testsuites>"} {
		if _, err := parseJUnit([]byte(bad)); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
}
