// SPDX-License-Identifier: Apache-2.0

package qualify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/ekaynac/auto-dev-cycle/internal/review"
)

// Workspace makes dir case c's candidate tree: the project's repository,
// its task's visible overlay (the task's base state), then c's patch,
// applied by git apply. env is the commands' whole environment.
func (s Suite) Workspace(ctx context.Context, dir string, c Case, env []string) error {
	if err := copyTree(filepath.Join(s.Root, s.Project, "repo"), dir); err != nil {
		return err
	}
	if v := filepath.Join(s.taskDir(c), "visible"); exists(v) {
		if err := copyTree(v, dir); err != nil {
			return err
		}
	}
	cmd := exec.CommandContext(ctx, "git", "apply", "--whitespace=nowarn", filepath.Join(s.Root, Dir, "cases", c.ID, "candidate.patch"))
	cmd.Dir, cmd.Env = dir, env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qualify: case %s's patch does not apply: %v: %s", c.ID, err, out)
	}
	return nil
}

// Prompt is case c's review prompt: its task's requirement, its patch as
// the diff, and the gate's summary on its candidate tree, made in dir.
func (s Suite) Prompt(ctx context.Context, c Case, gobin, dir string, env []string) ([]byte, error) {
	req, err := s.Requirement(c)
	if err != nil {
		return nil, err
	}
	diff, err := s.Patch(c)
	if err != nil {
		return nil, err
	}
	if err := s.Workspace(ctx, dir, c, env); err != nil {
		return nil, err
	}
	g, err := Gate(ctx, gobin, dir, env)
	if err != nil {
		return nil, err
	}
	return review.Prompt(review.PromptInput{Requirement: req, Diff: string(diff), Gate: g})
}

// Gate runs gobin's `go test -count=1 -json ./...` in dir with env, and is
// its summary: each test's last action counted once.
func Gate(ctx context.Context, gobin, dir string, env []string) (review.Gate, error) {
	cmd := exec.CommandContext(ctx, gobin, "test", "-count=1", "-json", "./...")
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.Output()
	if _, exit := err.(*exec.ExitError); err != nil && !exit {
		return review.Gate{}, err
	}
	last := map[string]string{}
	for sc := bufio.NewScanner(bytes.NewReader(out)); sc.Scan(); {
		var ev struct{ Action, Package, Test string }
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Test == "" {
			continue
		}
		if ev.Action == "pass" || ev.Action == "fail" || ev.Action == "skip" {
			last[ev.Package+"\x00"+ev.Test] = ev.Action
		}
	}
	var g review.Gate
	for _, a := range last {
		g.Passed, g.Failed, g.Skipped = g.Passed+b2i(a == "pass"), g.Failed+b2i(a == "fail"), g.Skipped+b2i(a == "skip")
	}
	g.Green = err == nil && g.Failed == 0 && g.Passed > 0
	return g, nil
}

// copyTree copies the regular files and directories under from into to.
func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		switch dst := filepath.Join(to, rel); {
		case e.IsDir():
			return os.MkdirAll(dst, 0o755)
		case !e.Type().IsRegular():
			return fmt.Errorf("qualify: %s is not a regular file", p)
		default:
			b, err := os.ReadFile(p)
			if err == nil {
				err = os.WriteFile(dst, b, 0o644)
			}
			return err
		}
	})
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
