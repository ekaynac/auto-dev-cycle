// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// startBox is a sandbox whose attempts fail with newErr, and whose every
// run ends with err, run or not.
type startBox struct {
	t      *testing.T
	newErr error
	err    error
}

func (b startBox) NewAttempt() (*sandbox.Attempt, error) {
	if b.newErr != nil {
		return nil, b.newErr
	}
	a := &sandbox.Attempt{Dir: b.t.TempDir()}
	return a, errors.Join(os.Mkdir(a.Workspace(), 0o700), os.Mkdir(a.Results(), 0o700))
}

func (b startBox) Start(context.Context, sandbox.Run) ([]byte, error) { return nil, b.err }

// exitErr is a real exit error of a process that exited code, or was
// killed (-1).
func exitErr(t *testing.T, code int) *exec.ExitError {
	t.Helper()
	script := fmt.Sprintf("exit %d", code)
	if code < 0 {
		script = "kill -KILL $$"
	}
	var exit *exec.ExitError
	if !errors.As(exec.Command("sh", "-c", script).Run(), &exit) {
		t.Fatalf("no exit error for %d", code)
	}
	return exit
}

// Astra chunk 1 H2 and round 2 (Lead decision, #226 entry 147): a listing's
// failure is the candidate's (a block) only on positive evidence: go
// list's own exit status observed and nonzero (sandbox.WorkloadExit), or a
// bound its run broke (sandbox.ErrLeft). Everything else is ErrGuardRun,
// never a block: the attempt not made (ENOSPC), the sandbox refusing the
// run (a mount sandbox.ErrSpec, sandbox.ErrHost), podman failing itself
// (125), its client killed (-1), no status, and a good run whose container
// was not removed (sandbox.ErrCleanup). The child killed before its result
// is ErrGuardRun too.
func TestAGuardThatDidNotRunIsAnError(t *testing.T) {
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		plain(t, src, "add", "--all")
	})
	change := map[string]string{"pkg/calc.go": strings.Replace(baseFiles["pkg/calc.go"], "package pkg\n", "package pkg\n\nimport \"fmt\"\n", 1) +
		"\nfunc Sum(xs ...int) int { return len(fmt.Sprint(xs)) }\n"}
	for name, c := range map[string]struct {
		box   startBox
		block bool
	}{
		"the attempt not made (ENOSPC)":   {box: startBox{newErr: &os.PathError{Op: "mkdir", Path: "runs/a1", Err: syscall.ENOSPC}}},
		"a mount the sandbox refused":     {box: startBox{err: fmt.Errorf("%w: a mount source holds a comma", sandbox.ErrSpec)}},
		"the sandbox refused the run":     {box: startBox{err: fmt.Errorf("%w: no image", sandbox.ErrHost)}},
		"podman failed itself":            {box: startBox{err: fmt.Errorf("podman: %w", exitErr(t, 125))}},
		"its client killed":               {box: startBox{err: fmt.Errorf("podman: %w", exitErr(t, -1))}},
		"no status came back":             {box: startBox{err: errors.New("podman: exec: no such file")}},
		"a good run, its container stays": {box: startBox{err: fmt.Errorf("%w: %w", sandbox.ErrCleanup, exitErr(t, 1))}},
		"go list exited 1, its container stays": {box: startBox{err: errors.Join(&sandbox.WorkloadExit{Code: 1, Err: exitErr(t, 1)},
			fmt.Errorf("%w: %w", sandbox.ErrCleanup, exitErr(t, 1)))}},
		"go list exited 1 on its code": {box: startBox{err: &sandbox.WorkloadExit{Code: 1, Err: exitErr(t, 1)}}, block: true},
		"its run left past its bound":  {box: startBox{err: errors.Join(sandbox.ErrLeft, sandbox.ErrSpec)}, block: true},
	} {
		t.Run(name, func(t *testing.T) {
			c.box.t = t
			v, err := f.loadedErr(t, boxLister(c.box, sandbox.Image{}, sandbox.Limits{}), change)
			switch {
			case c.block && (err != nil || !finds(v.Blocks, "go list in the prepared image")):
				t.Errorf("go list failing on the candidate: %v, %q", err, whats(v.Blocks))
			case !c.block && (!errors.Is(err, ErrGuardRun) || len(v.Blocks) != 0):
				t.Errorf("a go list that did not run to its own end: %v, %q", err, whats(v.Blocks))
			}
		})
	}
	t.Run("its scratch directory not made", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Accept(context.Background(), In{Repo: f.repo, Base: f.base, Patch: f.patch(t, change), Policy: f.policy,
			Guard: Integrity(f.repo, f.base, testLister(t), filepath.Join(file, "scratch"))})
		if !errors.Is(err, ErrGuardRun) {
			t.Errorf("a scratch directory not made: %v", err)
		}
	})
	t.Run("the child killed", func(t *testing.T) {
		procStarted = func(p *os.Process) { _ = p.Kill() }
		t.Cleanup(func() { procStarted = nil })
		if _, err := f.loadedErr(t, testLister(t), change); !didNotRunErr(err, "the guard's process ended without its result") {
			t.Errorf("a killed child: %v", err)
		}
	})
}
