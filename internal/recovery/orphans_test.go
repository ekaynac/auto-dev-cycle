// SPDX-License-Identifier: Apache-2.0

//go:build linux

package recovery_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/recovery"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const noBytes = types.Digest("sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")

// orphans runs the orphan stage over the exchange directory ex.
func (w *world) orphans(ex string) error {
	r := w.recovery()
	r.Exchanges = ex
	return r.RefuseOrphans(context.Background())
}

// root makes attempt id's exchange root in ex as adapterhost does (0700, in/ sealed, out/), with out/'s entries
// (name → contents), and returns it.
func root(t *testing.T, ex string, id types.AttemptID, out map[string]string) string {
	t.Helper()
	r := filepath.Join(ex, adapterhost.ExchangeName(id))
	err := errors.Join(os.Mkdir(r, 0o700), os.Mkdir(filepath.Join(r, "in"), 0o755), os.WriteFile(filepath.Join(r, "in", "prompt"), []byte("p"), 0o444),
		os.Chmod(filepath.Join(r, "in"), 0o555), os.Mkdir(filepath.Join(r, "out"), 0o755))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(r, "in"), 0o755) }) // a skipped test leaves the root to TempDir
	for n, body := range out {
		err = errors.Join(err, os.WriteFile(filepath.Join(r, "out", n), []byte(body), 0o644))
	}
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// refusals is every attempt.output_refused after seq, as "attempt handle size detail".
func (w *world) refusals(after int64) []string {
	var got []string
	for _, ev := range w.events() {
		var r types.AttemptOutputRefusal
		if ev.Seq > after && ev.Kind == lease.OutputRefused && json.Unmarshal(ev.Payload, &r) == nil && r.Handle.Digest == noBytes && r.Check == types.AttemptOutputRefusalCheckStaleAttempt {
			got = append(got, fmt.Sprintf("%s %s %d %s", r.Attempt, r.Handle.Handle, r.Handle.Size, r.Detail))
		}
	}
	slices.Sort(got)
	return got
}

// settled fails unless lease l settled.
func (w *world) settled(l types.LeaseID) {
	w.t.Helper()
	w.view(func(tx *eventstore.Tx) error {
		got, _, _, err := lease.LeaseOf(tx, l)
		if got.State != types.LeaseStateSettled {
			w.t.Errorf("lease %s is %s; want settled", l, got.State)
		}
		return err
	})
}

func empty(t *testing.T, ex string) {
	t.Helper()
	if entries, err := os.ReadDir(ex); err != nil || len(entries) != 0 {
		t.Fatalf("left in the exchange directory: %v %v", entries, err)
	}
}

// F (P21b, X3 "Late output"): output left by a cut attempt is refused, one attempt.output_refused{stale_attempt} per
// entry of out/, and never read: an entry is named by its lstat size with the digest of no bytes, and a FIFO, which
// an open would block on, is refused like a file. A1 becomes untrustworthy; the root is gone, and a second start
// records nothing. Past 64 entries, one more refusal marks the truncation.
func TestOrphanedOutputIsRefusedNeverRead(t *testing.T) {
	w, ex := restarted(t, t.TempDir()), t.TempDir()
	c := w.midCall(t)
	r := root(t, ex, c.Attempt, map[string]string{"partial.patch": "7 bytes"})
	if err := unix.Mkfifo(filepath.Join(r, "out", "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.orphans(ex); err == nil || !strings.Contains(err.Error(), "is running") {
		t.Fatalf("the orphan stage before step 1: %v", err)
	}
	w.run()
	mark := head(w)
	if err := w.orphans(ex); err != nil {
		t.Fatal(err)
	}
	detail := fmt.Sprintf("attempt %s is sealed; its publication is closed; not read", c.Attempt)
	if got := w.refusals(mark); !slices.Equal(got, []string{fmt.Sprintf("%s partial.patch 7 %s", c.Attempt, detail), fmt.Sprintf("%s pipe 0 %s", c.Attempt, detail)}) {
		t.Fatalf("refused %q", got)
	}
	w.view(func(tx *eventstore.Tx) error {
		if bad, err := lease.Untrustworthy(tx, c.Attempt); !bad || err != nil {
			t.Errorf("A1 untrustworthy: %v, %v", bad, err)
		}
		return nil
	})
	empty(t, ex)
	mark = head(w)
	if err := w.orphans(ex); err != nil || head(w) != mark {
		t.Fatalf("a second start: %v, %v", err, w.refusals(mark))
	}

	c2 := w.midCall(t)
	many := map[string]string{}
	for i := range 65 {
		many[fmt.Sprintf("f%02d", i)] = ""
	}
	root(t, ex, c2.Attempt, many)
	w.run()
	if err := w.orphans(ex); err != nil {
		t.Fatal(err)
	}
	if got := w.refusals(mark); len(got) != 65 || !slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, "; not read; out/ truncated at 64 entries") }) {
		t.Fatalf("65 entries refused as %d, with no truncation marker or a wrong count", len(got))
	}
}

// F (P21b; M1, M2, M3 of Opus on #259): a root is bound through the store, never through its name. No event is
// recorded, and every root is removed, for: A1's root renamed after F, an attempt that is finishing (its output was
// admitted); a root named after an attempt never minted; and the cut attempt's own root, either at mode 0755 or
// replaced by a symlink to a directory outside (0700, the controller's), which is not followed: its entries stay.
func TestARootIsBoundThroughTheStore(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprint("symlink ", symlink), func(t *testing.T) {
			w, ex, outside := restarted(t, t.TempDir()), t.TempDir(), t.TempDir()
			cut, finishing := w.midCall(t), w.midCall(t)
			w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.SubmitResult(tx, finishing.Attempt, 1, types.SubmittedResult{Call: finishing.Call, Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean})
			})
			root(t, ex, finishing.Attempt, map[string]string{"renamed": "A1's"})
			root(t, ex, types.AttemptID(newID(t, "att")), map[string]string{"forged": "x"})
			var err error
			if symlink {
				err = errors.Join(os.Chmod(outside, 0o700), os.MkdirAll(filepath.Join(outside, "out"), 0o755), os.WriteFile(filepath.Join(outside, "out", "kept"), nil, 0o600),
					os.Symlink(outside, filepath.Join(ex, adapterhost.ExchangeName(cut.Attempt))))
			} else {
				err = os.Chmod(root(t, ex, cut.Attempt, map[string]string{"x": "x"}), 0o755)
			}
			if err != nil {
				t.Fatal(err)
			}
			w.run()
			mark := head(w)
			if err := w.orphans(ex); err != nil {
				t.Fatal(err)
			}
			if got := w.refusals(0); head(w) != mark || len(got) != 0 {
				t.Fatalf("recorded %q", got)
			}
			empty(t, ex)
			if _, err := os.Lstat(filepath.Join(outside, "out", "kept")); symlink && err != nil {
				t.Fatalf("the symlink's target: %v", err)
			}
		})
	}
}

// sparse makes out/huge a sparse file of 2^54 bytes in root r (no space used), or skips where the file system refuses
// that size (ext4; the test sandbox's /tmp is a tmpfs).
func sparse(t *testing.T, r string) {
	t.Helper()
	p := filepath.Join(r, "out", "huge")
	if err := errors.Join(os.WriteFile(p, nil, 0o644), os.Truncate(p, 1<<54)); errors.Is(err, unix.EFBIG) {
		t.Skip("this file system refuses a 2^54-byte file")
	} else if err != nil {
		t.Fatal(err)
	}
}

// F (P21b; H1 and M6 of Opus on #259, M1, M2 and L6 of Opus on #261): the orphan stage never blocks recovery. Run,
// with the hostile root in its exchange directory, settles L1 and never touches the directory. The stage then records
// the refusals and clears the directory: an out/ at mode 000, or a symlink, is one refusal "out/" with the errno; a
// sparse 2^54-byte file is recorded at Amount's maximum, saying so; a refusal the store rejects (the clamp off) falls
// back to one refusal "out/"; a failed rename is reported, and the next start removes the root without recording its
// refusal again, as after a crash between the commit and the rename; a leftover refused-<name> never blocks it.
func TestTheOrphanStageNeverBlocksRecovery(t *testing.T) {
	const partial = "%[1]s partial 4 %[2]s; not read"
	for name, c := range map[string]struct {
		fault func(t *testing.T, ex, root string)
		want  []string
	}{
		"out/ at mode 000": {func(t *testing.T, _, r string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a directory at mode 000")
			}
			if err := os.Chmod(filepath.Join(r, "out"), 0); err != nil {
				t.Fatal(err)
			}
		}, []string{"%[1]s out/ 0 %[2]s; not read; out/ not listed: EACCES"}},
		"out/ a symlink": {func(t *testing.T, _, r string) {
			if err := errors.Join(os.Rename(filepath.Join(r, "out"), filepath.Join(r, "elsewhere")), os.Symlink("elsewhere", filepath.Join(r, "out"))); err != nil {
				t.Fatal(err)
			}
		}, []string{"%[1]s out/ 0 %[2]s; not read; out/ not listed: ENOTDIR"}},
		"a sparse file": {func(t *testing.T, _, r string) { sparse(t, r) },
			[]string{"%[1]s huge 9007199254740991 %[2]s; not read; lstat size 18014398509481984, recorded as 9007199254740991", partial}},
		"a refusal the store rejects": {func(t *testing.T, _, r string) {
			old := *recovery.MaxSize
			*recovery.MaxSize = 1 << 62
			t.Cleanup(func() { *recovery.MaxSize = old })
			sparse(t, r)
		}, []string{"%[1]s out/ 0 %[2]s; not read; out/ not recorded entry by entry"}},
		"a failed rename": {func(t *testing.T, ex, r string) {
			old := *recovery.Renameat
			*recovery.Renameat = func(int, string, int, string) error { *recovery.Renameat = old; return unix.EIO }
			t.Cleanup(func() { *recovery.Renameat = old })
		}, []string{partial}},
		"a leftover refused-<name>": {func(t *testing.T, ex, r string) {
			if err := os.MkdirAll(filepath.Join(ex, "refused-"+filepath.Base(r), "x"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, []string{partial}},
	} {
		t.Run(name, func(t *testing.T) {
			w, ex := restarted(t, t.TempDir()), t.TempDir()
			cut := w.midCall(t)
			r := root(t, ex, cut.Attempt, map[string]string{"partial": "part"})
			c.fault(t, ex, r)
			rec := w.recovery()
			rec.Exchanges = ex
			if err := rec.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			w.settled(cut.Lease)
			if _, err := os.Lstat(r); err != nil || len(w.refusals(0)) != 0 {
				t.Fatalf("Run touched the exchange directory: %v, refused %q", err, w.refusals(0))
			}
			err := rec.RefuseOrphans(context.Background())
			if name == "a failed rename" {
				if err == nil || !strings.Contains(err.Error(), "the orphaned output of "+string(cut.Attempt)) {
					t.Fatalf("the failed rename: %v", err)
				}
				err = rec.RefuseOrphans(context.Background()) // the next start
			}
			var want []string
			for _, f := range c.want {
				want = append(want, fmt.Sprintf(f, cut.Attempt, fmt.Sprintf("attempt %s is sealed; its publication is closed", cut.Attempt)))
			}
			if got := w.refusals(0); err != nil || !slices.Equal(got, want) {
				t.Fatalf("%v: refused %q; want %q", err, got, want)
			}
			empty(t, ex)
		})
	}
}

// M1 and L4 (Opus on #261): at most maxRoots roots a start are refused, and a root that fails takes no slot: with one
// slot, the first root's rename fails and the second root is still refused in the same start; the next start takes
// the first. An attempt stopped without a result (stop_reason set) is in the cut set: its output is refused too.
func TestARootThatFailsTakesNoSlot(t *testing.T) {
	old, oldRename := *recovery.MaxRoots, *recovery.Renameat
	t.Cleanup(func() { *recovery.MaxRoots, *recovery.Renameat = old, oldRename })
	*recovery.MaxRoots = 1
	w, ex := restarted(t, t.TempDir()), t.TempDir()
	first := w.midCall(t)
	stopped := w.midCall(t)
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.StopRunning(tx, stopped.Attempt, types.AttemptStopReasonValueRevoke)
	})
	root(t, ex, first.Attempt, map[string]string{"a": "1"})
	root(t, ex, stopped.Attempt, map[string]string{"b": "22"})
	*recovery.Renameat = func(fd int, from string, tfd int, to string) error {
		if from == adapterhost.ExchangeName(first.Attempt) {
			return unix.EIO
		}
		return oldRename(fd, from, tfd, to)
	}
	w.run()
	if err := w.orphans(ex); err == nil {
		t.Fatal("the first root's failed rename is not reported")
	}
	if got := w.refusals(0); len(got) != 2 || !strings.HasPrefix(got[1], string(stopped.Attempt)+" b 2 attempt "+string(stopped.Attempt)+" is sealed") {
		t.Fatalf("refused %q; want both roots' outputs, the stopped attempt's included", got)
	}
	*recovery.Renameat = oldRename
	if err := w.orphans(ex); err != nil || len(w.refusals(0)) != 2 {
		t.Fatalf("the next start: %v, refused %q", err, w.refusals(0))
	}
	empty(t, ex)
}
