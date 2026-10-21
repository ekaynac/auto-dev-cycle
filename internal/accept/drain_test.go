// SPDX-License-Identifier: Apache-2.0

//go:build linux

package accept

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// sh is a controlled subprocess under a deadline.
func sh(t *testing.T, ctx context.Context, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
	return cmd
}

// Astra round 2 (M2, L5): a command writing 64 MiB, on stdout or on
// stderr with little stdout, is cut at its 64 KiB bound, keeps nothing,
// and the overflow is ErrTooLarge. The command then outlives the closed
// pipe (SIGPIPE ignored, a sleep holding the pipes), so only the kill
// ends it promptly, well within the deadline. Consumption is bounded: the
// writer is killed mid-flood, so it never reaches the line after it (an
// unbounded read followed by a size check lets the flood finish). The
// writer is finite, so such a mutation fails here instead of exhausting
// memory.
func TestDrainBoundsBothStreams(t *testing.T) {
	for name, script := range map[string]string{
		"stdout": `trap '' PIPE; head -c 67108864 /dev/zero; touch "$M"; sleep 20`,
		"stderr": `trap '' PIPE; echo little; head -c 67108864 /dev/zero >&2; touch "$M"; sleep 20`,
	} {
		marker := filepath.Join(t.TempDir(), "flooded")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		start := time.Now()
		out, stderr, err := drain(ctx, sh(t, ctx, "M="+marker+"; "+script), 64<<10, 64<<10)
		cancel()
		// Promptly: within a few times the same flood with no sleep after it,
		// timed now (#226 entry 89: no fixed wall-clock bound), and a pipe
		// delay; waiting for the sleep would take the deadline's 10 s.
		took := time.Since(start)
		bctx, bcancel := context.WithTimeout(context.Background(), 10*time.Second)
		bstart := time.Now()
		_, _, _ = drain(bctx, sh(t, bctx, strings.ReplaceAll(strings.ReplaceAll(script, `; touch "$M"; sleep 20`, ""), "trap '' PIPE; ", "")), 64<<10, 64<<10)
		base := time.Since(bstart)
		bcancel()
		if !errors.Is(err, ErrTooLarge) || out != nil || stderr != nil || took > 4*base+pipeDelay {
			t.Errorf("%s: %v after %v (%d, %d bytes kept); want ErrTooLarge promptly", name, err, took, len(out), len(stderr))
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%s: the whole 64 MiB flood was consumed", name)
		}
	}
	out, stderr, err := drain(context.Background(), sh(t, context.Background(), "echo out; echo err >&2"), 64<<10, 64<<10)
	if err != nil || string(out) != "out\n" || string(stderr) != "err\n" {
		t.Errorf("an ordinary command: %q, %q, %v", out, stderr, err)
	}
}

// descendant starts script with $P naming a file its shell writes a
// descendant's pid into, and returns a way to read that pid. Every pid it
// reads is killed when the test ends.
func descendant(t *testing.T, script string) (*exec.Cmd, func() int) {
	t.Helper()
	pidfile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	cmd := sh(t, ctx, "P="+pidfile+"; "+script)
	var pid int
	t.Cleanup(func() {
		cancel()
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return cmd, func() int {
		b, err := os.ReadFile(pidfile)
		if pid, err = strconv.Atoi(strings.TrimSpace(string(b))); err != nil || pid <= 1 {
			t.Fatalf("the fixture wrote no pid: %q, %v", b, err)
		}
		return pid
	}
}

// gone: pid has exited and been reaped, within a few seconds.
func gone(pid int) bool {
	for range 60 {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// Astra round 3 (Low): an overflow kills the whole process group, not
// only the leader. The descendant holds no pipe (it is not ended by a
// closed pipe) and recorded its pid before the flood.
func TestDrainKillsTheGroup(t *testing.T) {
	cmd, pid := descendant(t, `sleep 30 </dev/null >/dev/null 2>&1 & echo $! > "$P"; head -c 67108864 /dev/zero; sleep 30`)
	if _, _, err := drain(context.Background(), cmd, 64<<10, 64<<10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("drain: %v, want ErrTooLarge", err)
	}
	if p := pid(); !gone(p) {
		t.Errorf("the group's descendant %d survived the overflow", p)
	}
}

// Astra round 3 (Low): a leader that exits while a descendant holds its
// pipe ends drain within pipeDelay, with its output, not when the
// descendant exits.
func TestDrainCutsInheritedPipes(t *testing.T) {
	bstart := time.Now() // the same leader with no descendant, timed now (#226 entry 89)
	_, _, _ = drain(context.Background(), sh(t, context.Background(), `echo done`), 64<<10, 64<<10)
	base := time.Since(bstart)
	cmd, pid := descendant(t, `sleep 10 & echo $! > "$P"; echo done`)
	start := time.Now()
	out, _, err := drain(context.Background(), cmd, 64<<10, 64<<10)
	if took := time.Since(start); err != nil || string(out) != "done\n" || took > 2*pipeDelay+4*base {
		t.Errorf("drain: %q, %v after %v; want done within pipeDelay", out, err, took)
	}
	pid() // read for cleanup
}

// Astra round 2 (M4): a tree object over the bound, with the file list
// within it, is the candidate's unsafe_output, with no accepted tree and
// no controller error.
func TestTreeObjectOverBound(t *testing.T) {
	f := newFixtureWith(t, func(src string) {
		wide := map[string]string{}
		for i := range 60 {
			wide[fmt.Sprintf("wide-file-%02d.txt", i)] = "x\n"
		}
		write(t, src, wide)
		plain(t, src, "add", "--all")
	})
	patch := f.patch(t, map[string]string{"money.py": "x = 1\n"})
	old := maxPatch
	maxPatch = 1024 // the patch and its one-entry file list fit; the root tree does not
	t.Cleanup(func() { maxPatch = old })
	v, err := Accept(context.Background(), In{Repo: f.repo, Base: f.base, Patch: patch, Policy: f.policy})
	if err != nil || v.Reason() != vocab.ReasonUnsafeOutput || v.Tree != "" || v.TreeDigest != types.Digest("") || v.Files != 1 {
		t.Fatalf("a wide tree: %v, tree %q, files %d, %v; want unsafe_output", reasons(v.Blocks), v.Tree, v.Files, err)
	}
}
