// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

const goMiniT0 = "../../fixtures/t0/go-mini"

// baseRemote runs scripts/lib/base-remote.sh's base_remote on repo, task
// and overlay into a new bare remote, as the driver calls it (under
// errexit, in a command substitution with || after it), with env added and
// TMPDIR a directory of the test's; it returns the tree it prints, the
// remote and the TMPDIR.
func baseRemote(t *testing.T, repo, task, overlay string, env ...string) (string, string, string, error) {
	t.Helper()
	dest, tmp := filepath.Join(t.TempDir(), "remote.git"), t.TempDir()
	cmd := exec.Command("bash", "-c", `set -euo pipefail; . ../../scripts/lib/base-remote.sh; tree="$(base_remote "$@")" || exit 1; echo "$tree"`,
		"base_remote", repo, filepath.Join(goMiniT0, "tasks", task), overlay, dest)
	cmd.Env = append(append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "TMPDIR="+tmp), env...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), dest, tmp, err
}

// P24a: the base remote of go-mini-feature-1 is T0 go-mini's repo/ as
// committed, the tree its measured baseline binds
// (fixtures/m1/stub/go-mini/baseline.json); a task's visible/ overlay and a
// case's base_overlay change it, each file landing at its path; an existing
// destination is refused; a copy that fails fails it, with no remote; its
// work directory is gone on every path; and a caller's GIT_DIR does not
// redirect it.
func TestBaseRemote(t *testing.T) {
	var rec struct {
		BaseTree string `json:"base_tree"`
	}
	raw, err := os.ReadFile("../../fixtures/m1/stub/go-mini/baseline.json")
	if err == nil {
		err = json.Unmarshal(raw, &rec)
	}
	if err != nil {
		t.Fatal(err)
	}
	repo := goMiniT0 + "/repo"
	empty := func(what, dir string) {
		if left, err := os.ReadDir(dir); err != nil || len(left) != 0 {
			t.Errorf("%s: its TMPDIR holds %v, %v", what, left, err)
		}
	}
	other := filepath.Join(t.TempDir(), "other.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", other).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	tree, remote, tmp, err := baseRemote(t, repo, "go-mini-feature-1", "", "GIT_DIR="+other)
	if err != nil || tree != rec.BaseTree {
		t.Fatalf("go-mini-feature-1's base remote: tree %q, %v; the baseline's %s", tree, err, rec.BaseTree)
	}
	empty("go-mini-feature-1", tmp)
	gitIn := func(dir string, args ...string) string {
		cmd := exec.Command("git", append([]string{"--git-dir", dir}, args...)...)
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, _ := cmd.Output()
		return strings.TrimSpace(string(out))
	}
	if head, count := gitIn(remote, "symbolic-ref", "HEAD"), gitIn(remote, "rev-list", "--count", "main"); head != "refs/heads/main" || count != "1" {
		t.Errorf("the remote: HEAD %s, %s commits on main", head, count)
	}
	if refs := gitIn(other, "for-each-ref"); refs != "" {
		t.Errorf("the caller's GIT_DIR got %s", refs)
	}
	visible, remote, _, err := baseRemote(t, repo, "go-mini-failing-then-green-1", "")
	const shown = "inventory/money_negative_test.go"
	want, _ := os.ReadFile(filepath.Join(goMiniT0, "tasks/go-mini-failing-then-green-1/visible", shown))
	if err != nil || visible == rec.BaseTree || len(want) == 0 || gitIn(remote, "show", "main:"+shown) != strings.TrimSpace(string(want)) {
		t.Errorf("a task with visible/: tree %s, %v; %s not at its path", visible, err, shown)
	}
	overlay := t.TempDir()
	err = os.MkdirAll(filepath.Join(overlay, "docs"), 0o755)
	for name, body := range map[string]string{"conftest.go": "package conftest\n", "docs/AGENTS.md.hostile": "hostile\n"} {
		err = errors.Join(err, os.WriteFile(filepath.Join(overlay, name), []byte(body), 0o644))
	}
	if err = errors.Join(err, os.Symlink("../conftest.go", filepath.Join(overlay, "docs/key"))); err != nil {
		t.Fatal(err)
	}
	over, remote, _, err := baseRemote(t, repo, "go-mini-feature-1", overlay)
	if err != nil || over == rec.BaseTree || gitIn(remote, "show", "main:conftest.go") != "package conftest" {
		t.Errorf("a base overlay: tree %s, %v", over, err)
	}
	// P24c: a symlink lands as a symlink; NAME.hostile lands as NAME.
	if docs := gitIn(remote, "ls-tree", "main", "docs/"); !strings.Contains(docs, "120000 blob") || !strings.HasSuffix(docs, "\tdocs/key") ||
		!strings.Contains(docs, "\tdocs/AGENTS.md\n") || strings.Contains(docs, ".hostile") {
		t.Errorf("a base overlay's link and .hostile file: %q", docs)
	}
	cmd := exec.Command("bash", "-c", `. ../../scripts/lib/base-remote.sh && base_remote "$@"`, "base_remote", repo,
		goMiniT0+"/tasks/go-mini-feature-1", "", remote)
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
	if err := cmd.Run(); err == nil {
		t.Error("an existing destination was not refused")
	}
	if os.Geteuid() == 0 {
		return // root reads a file at mode 000
	}
	broken := t.TempDir()
	for name, mode := range map[string]os.FileMode{"a.txt": 0o644, "b.txt": 0} {
		if err := os.WriteFile(filepath.Join(broken, name), []byte(name), mode); err != nil {
			t.Fatal(err)
		}
	}
	tree, remote, tmp, err = baseRemote(t, broken, "go-mini-feature-1", "")
	if _, statErr := os.Stat(remote); err == nil || tree != "" || statErr == nil {
		t.Errorf("a repo with an unreadable file: tree %q, %v; the remote %v", tree, err, statErr)
	}
	empty("a failed copy", tmp)
}

// TestCanaryPath (P24c): canary_path finds a canary path anywhere under the
// work directory, or beside it, never deeper beside it, and passes when
// there is none.
func TestCanaryPath(t *testing.T) {
	parent := t.TempDir()
	work := filepath.Join(parent, "work")
	for _, d := range []string{"work/state/run/tree", "other"} {
		if err := os.MkdirAll(filepath.Join(parent, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	plant := map[string]string{"canary-deep": "work/state/canary-deep", "canary-beside": "canary-beside", "canary-far": "other/canary-far"}
	for _, p := range plant {
		if err := os.WriteFile(filepath.Join(parent, p), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]string{"canary-deep": plant["canary-deep"], "canary-beside": plant["canary-beside"], "canary-far": "", "canary-none": ""} {
		cmd := exec.Command("bash", "-c", `. ../../scripts/lib/base-remote.sh && canary_path "$@"`, "canary_path", work, name)
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
		out, err := cmd.Output()
		if got := strings.TrimSpace(string(out)); want == "" && (err != nil || got != "") || want != "" && (err == nil || got != filepath.Join(parent, want)) {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
}
