// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"testing"
)

// exitOf is a real exit error: code, or -1 for a process killed.
func exitOf(t *testing.T, code int) error {
	t.Helper()
	script := fmt.Sprintf("exit %d", code)
	if code < 0 {
		script = "kill -KILL $$"
	}
	err := exec.Command("sh", "-c", script).Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("no exit error for %d", code)
	}
	return err
}

// Lead decision (#226 entry 147): Start reports the workload's own exit
// status (WorkloadExit) only when it was observed: podman run's status
// 1-124 or a signal's 128 and above. Its client killed (-1) or failing
// itself (125-127) is none, and the container's removal failing after the
// run is ErrCleanup, apart, never the workload's.
func TestStartReportsTheWorkloadsStatusApart(t *testing.T) {
	for name, c := range map[string]struct {
		run, rm  error
		workload int // 0: none
		cleanup  bool
	}{
		"the workload exited 1":         {run: exitOf(t, 1), workload: 1},
		"the workload's process killed": {run: exitOf(t, 137), workload: 137},
		"podman failed itself":          {run: exitOf(t, 125)},
		"its client killed":             {run: exitOf(t, -1)},
		"a good run, rm exiting 1":      {rm: exitOf(t, 1), cleanup: true},
		"the workload exited 1, rm 1":   {run: exitOf(t, 1), rm: exitOf(t, 1), workload: 1, cleanup: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{fn: func(_ context.Context, args []string) ([]byte, error) {
				switch {
				case slices.Contains(args, "run"):
					return nil, c.run
				case slices.Contains(args, "rm"):
					return nil, c.rm
				}
				return nil, nil
			}}
			h := testHost(t, f)
			_, err := h.Start(context.Background(), testRun(t, h, Gate))
			var w *WorkloadExit
			got := 0
			if errors.As(err, &w) {
				got = w.Code
			}
			if got != c.workload || errors.Is(err, ErrCleanup) != c.cleanup {
				t.Errorf("Start: workload %d, cleanup %v: %v", got, errors.Is(err, ErrCleanup), err)
			}
		})
	}
}
