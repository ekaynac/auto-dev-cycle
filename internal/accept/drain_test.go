// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
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
// ends it promptly, well within the deadline. The writer is finite, so a
// mutation that buffers without a bound fails here instead of exhausting
// memory.
func TestDrainBoundsBothStreams(t *testing.T) {
	for name, script := range map[string]string{
		"stdout": "trap '' PIPE; head -c 67108864 /dev/zero; sleep 20",
		"stderr": "trap '' PIPE; echo little; head -c 67108864 /dev/zero >&2; sleep 20",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		start := time.Now()
		out, stderr, err := drain(sh(t, ctx, script), 64<<10, 64<<10)
		cancel()
		if took := time.Since(start); !errors.Is(err, ErrTooLarge) || out != nil || stderr != nil || took > 3*time.Second {
			t.Errorf("%s: %v after %v (%d, %d bytes kept); want ErrTooLarge promptly", name, err, took, len(out), len(stderr))
		}
	}
	out, stderr, err := drain(sh(t, context.Background(), "echo out; echo err >&2"), 64<<10, 64<<10)
	if err != nil || string(out) != "out\n" || string(stderr) != "err\n" {
		t.Errorf("an ordinary command: %q, %q, %v", out, stderr, err)
	}
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
