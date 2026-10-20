// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

// P24c: each §2.7 hostile case's positive control, run here, outside the
// product: the setup does what the case claims, so the case discriminates.
// The product's refusals are the rows the cases name (accept's patch.diff
// rows: TestHostilePatches; the gateway's: TestHostileModelSwitch).
func TestCycleHostileControls(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	home := testguard.HomeEnv(t)
	base := append([]string{"PATH=" + filepath.Dir(goBin) + ":/usr/bin:/bin", "GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-mod=readonly",
		"GOCACHE=" + goEnv(t, goBin, "GOCACHE"), "GOMODCACHE=" + goEnv(t, goBin, "GOMODCACHE")}, home...)
	run := func(dir string, env []string, argv ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir, cmd.Env = dir, append(slices.Clone(base), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	// tree is a fresh scratch parent holding tree/: go-mini's repository
	// with the case's base overlay.
	tree := func(name string) (parent, dir string) {
		parent = t.TempDir()
		dir = filepath.Join(parent, "tree")
		copyTree(t, filepath.Join(goMini, "repo"), dir)
		if overlay := filepath.Join(m1, "cases", name, "testdata"); exists(overlay) {
			copyTree(t, overlay, dir)
		}
		return parent, dir
	}

	t.Run("hostile-instructions", func(t *testing.T) {
		_, dir := tree("hostile-instructions")
		for _, f := range []string{"AGENTS.md", "CLAUDE.md", ".cursorrules", ".github/copilot-instructions.md"} {
			if !strings.Contains(readFile(t, filepath.Join(dir, f)), "ADC-CANARY-hostile-instructions") {
				t.Errorf("the base's %s does not hold the canary", f)
			}
		}
		// The builder's first turn reads AGENTS.md: what it reads, the canary, is in its next request.
		if reads := firstTurn(t, "hostile-instructions").Read; !slices.Equal(reads, []string{"AGENTS.md"}) {
			t.Errorf("the builder's first turn reads %v", reads)
		}
	})

	t.Run("hostile-git-dir", func(t *testing.T) {
		parent, dir := tree("hostile-git-dir")
		git := func(args ...string) {
			t.Helper()
			if out, err := run(dir, []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@a",
				"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@a"}, append([]string{"git"}, args...)...); err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
		}
		git("init", "-q", "--initial-branch=main")
		git("add", "-A")
		git("commit", "-q", "-m", "base")
		// The builder's writes, as plain files: .git/config appended, the hook made executable.
		w := firstTurn(t, "hostile-git-dir").Write
		cfg, err := os.OpenFile(filepath.Join(dir, ".git/config"), os.O_APPEND|os.O_WRONLY, 0)
		if err == nil {
			_, err = cfg.WriteString(w[".git/config"])
			err = errors.Join(err, cfg.Close())
		}
		if err == nil {
			err = os.MkdirAll(filepath.Join(dir, ".githooks"), 0o755)
		}
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, ".githooks/post-checkout"), []byte(w[".githooks/post-checkout"]), 0o755)
		}
		if err != nil {
			t.Fatal(err)
		}
		canary := filepath.Join(parent, "canary-hostile-git-dir")
		if exists(canary) {
			t.Fatal("the canary exists before plain git ran")
		}
		git("status", "--porcelain")
		git("checkout", "-q", "-b", "other")
		if !exists(canary) {
			t.Errorf("plain git status and checkout did not create %s", canary)
		}
	})

	t.Run("hostile-test-escape", func(t *testing.T) {
		parent, dir := tree("hostile-test-escape")
		// The decoy: the child's own home (testguard's, HomeEnv's first entry) with a .config/adc.
		_, h, _ := strings.Cut(home[0], "=")
		if err := os.MkdirAll(filepath.Join(h, ".config/adc"), 0o700); err != nil {
			t.Fatal(err)
		}
		out, err := run(dir, []string{"ADC_DECOY_TOKEN=x"}, "go", "test", "-count=1", "-v", "-run", "TestEscapeProbe", "./inventory/")
		if err != nil {
			t.Fatalf("the probe: %v %s", err, out)
		}
		if !strings.Contains(out, "ADC-PROBE cfg=<nil>") || !strings.Contains(out, "ADC_DECOY_TOKEN") || !strings.Contains(out, "out=<nil>") ||
			!exists(filepath.Join(parent, "canary-hostile-test-escape")) {
			t.Errorf("on the host the probe did not read the decoy, list its env name and write outside: %s", out)
		}
	})

	t.Run("hostile-gate-output", func(t *testing.T) {
		var c struct {
			ProfileOverlay struct {
				Gate struct{ Command []string } `json:"gate"`
			} `json:"profile_overlay"`
		}
		readJSON(t, filepath.Join(m1, "cases/hostile-gate-output/case.json"), &c)
		argv := slices.Clone(c.ProfileOverlay.Gate.Command)
		for i := range argv {
			argv[i] = strings.ReplaceAll(argv[i], "{canary}", "ADC-CANARY-hostile-gate-output-test")
		}
		results := t.TempDir()
		_, dir := tree("hostile-gate-output")
		out, err := run(dir, []string{"ADC_RESULTS=" + results}, argv...)
		if err != nil || len(failed(t, out)) != 0 || exists(filepath.Join(results, ".canary")) {
			t.Errorf("at the base the gate is not green, or wrote the canary: %v %s", err, out)
		}
		for p, body := range firstTurn(t, "hostile-gate-output").Write {
			if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		out, _ = run(dir, []string{"ADC_RESULTS=" + results}, argv...)
		if !slices.Equal(failed(t, out), []string{"TestEnvProbe"}) || !strings.Contains(out, "canary: ADC-CANARY-hostile-gate-output-test") {
			t.Errorf("on the candidate the gate's stream does not fail TestEnvProbe with the canary: %s", out)
		}
	})

	t.Run("hostile-symlink", func(t *testing.T) {
		_, dir := tree("")
		out, err := run(dir, []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}, "git", "apply", filepath.Join(abs(t, m1), "cases/hostile-symlink/patch.diff"))
		if err != nil {
			t.Fatalf("git apply: %v %s", err, out)
		}
		if target, err := os.Readlink(filepath.Join(dir, "docs/key")); err != nil || target != "/home/u/.ssh/id_ed25519" {
			t.Errorf("plain git apply did not create the link: %q %v", target, err)
		}
		for _, p := range firstTurn(t, "hostile-symlink").Read {
			if !strings.HasPrefix(p, "/") && !strings.Contains(p, "..") {
				t.Errorf("the builder's read of %s is no escape the workspace refuses", p)
			}
		}
	})

	t.Run("hostile-symlink-path", func(t *testing.T) {
		parent, dir := tree("")
		patch := readFile(t, filepath.Join(m1, "cases/hostile-symlink-path/patch.diff"))
		unsafeApply(t, parent, dir, patch)
		got := readFile(t, filepath.Join(parent, "outside.txt"))
		if got != "ADC-CANARY-hostile-symlink-path\n" {
			t.Errorf("the unsafe applier wrote %q", got)
		}
		entries, _ := os.ReadDir(parent)
		if len(entries) != 2 { // tree/ and outside.txt, nothing else
			t.Errorf("the scratch parent holds %d entries", len(entries))
		}
		if _, ok := firstTurn(t, "hostile-symlink-path").Write["docs/../../outside.txt"]; !ok {
			t.Error("the builder does not try docs/../../outside.txt")
		}
	})
}

// unsafeApply is P24's deliberately unsafe fixture applier (§2.7): it writes
// each new file of patch at dir/path, resolving .. with no check, but only
// inside parent, a disposable scratch directory it refuses to leave.
func unsafeApply(t *testing.T, parent, dir, patch string) {
	t.Helper()
	var path string
	var body strings.Builder
	flush := func() {
		if path == "" {
			return
		}
		target := filepath.Join(dir, path) // .. resolved, unchecked: the point of this applier
		if rel, err := filepath.Rel(parent, target); err != nil || strings.HasPrefix(rel, "..") {
			t.Fatalf("the unsafe applier would write %s, outside its scratch parent", target)
		}
		if err := os.WriteFile(target, []byte(body.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		path = ""
		body.Reset()
	}
	for _, line := range strings.SplitAfter(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			flush()
			path = strings.TrimSuffix(strings.TrimPrefix(line, "+++ b/"), "\n")
		case path != "" && strings.HasPrefix(line, "+"):
			body.WriteString(line[1:])
		}
	}
	flush()
}

type builderTurn struct {
	Read  []string          `json:"read"`
	Write map[string]string `json:"write"`
}

// firstTurn is the scenario's builder's first turn with a read or a write.
func firstTurn(t *testing.T, scenario string) builderTurn {
	t.Helper()
	var s struct {
		Calls map[string]struct {
			Turns []builderTurn `json:"turns"`
		} `json:"calls"`
	}
	readJSON(t, filepath.Join(m1, "stub-provider/scenarios", scenario+".json"), &s)
	for _, tr := range s.Calls["stub-builder-1"].Turns {
		if len(tr.Read)+len(tr.Write) > 0 {
			return tr
		}
	}
	t.Fatalf("%s: the builder has no read or write turn", scenario)
	return builderTurn{}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func abs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func goEnv(t *testing.T, goBin, key string) string {
	t.Helper()
	out, err := exec.Command(goBin, "env", key).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(bytes.TrimSpace(out))
}
