// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

const goMiniT0 = "../../fixtures/t0/go-mini"

// baseRemote runs scripts/lib/base-remote.sh's base_remote on task and
// overlay into a new bare remote, and returns the tree it prints and the
// remote.
func baseRemote(t *testing.T, task, overlay string) (string, string, error) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "remote.git")
	cmd := exec.Command("bash", "-c", `. ../../scripts/lib/base-remote.sh && base_remote "$@"`, "base_remote",
		goMiniT0+"/repo", filepath.Join(goMiniT0, "tasks", task), overlay, dest)
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), dest, err
}

// P24a: the base remote of go-mini-feature-1 is T0 go-mini's repo/ as
// committed, the tree its measured baseline binds
// (fixtures/m1/stub/go-mini/baseline.json); a task's visible/ overlay and a
// case's base_overlay change it, each file landing at its path; and an
// existing destination is refused.
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
	tree, remote, err := baseRemote(t, "go-mini-feature-1", "")
	if err != nil || tree != rec.BaseTree {
		t.Fatalf("go-mini-feature-1's base remote: tree %q, %v; the baseline's %s", tree, err, rec.BaseTree)
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"--git-dir", remote}, args...)...)
		cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	if head, count := git("symbolic-ref", "HEAD"), git("rev-list", "--count", "main"); head != "refs/heads/main" || count != "1" {
		t.Errorf("the remote: HEAD %s, %s commits on main", head, count)
	}
	visible, _, err := baseRemote(t, "go-mini-failing-then-green-1", "")
	if err != nil || visible == rec.BaseTree {
		t.Errorf("a task with visible/: tree %s, %v", visible, err)
	}
	overlay := t.TempDir()
	if err := os.WriteFile(filepath.Join(overlay, "conftest.go"), []byte("package conftest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	over, remote, err := baseRemote(t, "go-mini-feature-1", overlay)
	if err != nil || over == rec.BaseTree || git("show", "main:conftest.go") != "package conftest" {
		t.Errorf("a base overlay: tree %s, %v", over, err)
	}
	cmd := exec.Command("bash", "-c", `. ../../scripts/lib/base-remote.sh && base_remote "$@"`, "base_remote", goMiniT0+"/repo",
		goMiniT0+"/tasks/go-mini-feature-1", "", remote)
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
	if err := cmd.Run(); err == nil {
		t.Error("an existing destination was not refused")
	}
}
