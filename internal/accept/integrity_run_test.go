// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// startBox is a sandbox whose every run ends with err, run or not.
type startBox struct {
	t   *testing.T
	err error
}

func (b startBox) NewAttempt() (*sandbox.Attempt, error) {
	a := &sandbox.Attempt{Dir: b.t.TempDir()}
	return a, errors.Join(os.Mkdir(a.Workspace(), 0o700), os.Mkdir(a.Results(), 0o700))
}

func (b startBox) Start(context.Context, sandbox.Run) ([]byte, error) { return nil, b.err }

// Astra chunk 1 H2: a guard that did not run to its verdict is ErrGuardRun,
// never a block the caller would select as the candidate's refusal:
//   - its child killed before its result;
//   - a sandbox run of go list that did not run: the sandbox refused it,
//     or podman failed itself (125), or no status came back.
//
// go list's own failure on the candidate's code (a run that exits with
// its own status) stays a block.
func TestAGuardThatDidNotRunIsAnError(t *testing.T) {
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		plain(t, src, "add", "--all")
	})
	change := map[string]string{"pkg/calc.go": strings.Replace(baseFiles["pkg/calc.go"], "package pkg\n", "package pkg\n\nimport \"fmt\"\n", 1) +
		"\nfunc Sum(xs ...int) int { return len(fmt.Sprint(xs)) }\n"}
	exit := func(code int) error {
		err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
		return fmt.Errorf("podman: %w", err)
	}
	for name, c := range map[string]struct {
		err   error
		block bool
	}{
		"the sandbox refused the run": {err: fmt.Errorf("%w: no image", sandbox.ErrHost)},
		"podman failed itself":        {err: exit(125)},
		"no status came back":         {err: errors.New("podman: exec: no such file")},
		"go list failed on its code":  {err: exit(1), block: true},
	} {
		t.Run(name, func(t *testing.T) {
			v, err := f.loadedErr(t, boxLister(startBox{t: t, err: c.err}, sandbox.Image{}, sandbox.Limits{}), change)
			switch {
			case c.block && (err != nil || !finds(v.Blocks, "go list in the prepared image")):
				t.Errorf("go list failing on the candidate: %v, %q", err, whats(v.Blocks))
			case !c.block && !didNotRunErr(err, "go list did not run in the sandbox"):
				t.Errorf("a go list that did not run: %v, %q", err, whats(v.Blocks))
			}
		})
	}
	t.Run("the child killed", func(t *testing.T) {
		procStarted = func(p *os.Process) { _ = p.Kill() }
		t.Cleanup(func() { procStarted = nil })
		if _, err := f.loadedErr(t, testLister(t), change); !didNotRunErr(err, "the guard's process ended without its result") {
			t.Errorf("a killed child: %v", err)
		}
	})
}
