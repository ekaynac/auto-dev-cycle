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

// F (P21b; H1 and M6 of Opus on #259): the orphan stage never blocks recovery. An out/ at mode 000, or a symlink, is
// one refusal, handle "out", with the errno, and its root is still removed; a failed rename is reported, and the next
// start removes the root without recording its refusal again, as after a crash between the commit and the rename.
// Each ends with the lease settled.
func TestTheOrphanStageNeverBlocksRecovery(t *testing.T) {
	for name, c := range map[string]struct {
		fault  func(t *testing.T, ex, root string)
		detail string
	}{
		"out/ at mode 000": {func(t *testing.T, _, r string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a directory at mode 000")
			}
			if err := os.Chmod(filepath.Join(r, "out"), 0); err != nil {
				t.Fatal(err)
			}
		}, " out 0 %s; not read; out/ not listed: EACCES"},
		"out/ a symlink": {func(t *testing.T, _, r string) {
			if err := errors.Join(os.Rename(filepath.Join(r, "out"), filepath.Join(r, "elsewhere")), os.Symlink("elsewhere", filepath.Join(r, "out"))); err != nil {
				t.Fatal(err)
			}
		}, " out 0 %s; not read; out/ not listed: ENOTDIR"},
		"a failed rename": {func(t *testing.T, ex, r string) {
			if err := os.MkdirAll(filepath.Join(ex, "refused-"+filepath.Base(r), "x"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, " partial 4 %s; not read"},
	} {
		t.Run(name, func(t *testing.T) {
			w, ex := restarted(t, t.TempDir()), t.TempDir()
			cut := w.midCall(t)
			r := root(t, ex, cut.Attempt, map[string]string{"partial": "part"})
			c.fault(t, ex, r)
			w.run()
			err := w.orphans(ex)
			if name == "a failed rename" {
				if err == nil || !strings.Contains(err.Error(), "the orphaned output of "+string(cut.Attempt)) {
					t.Fatalf("the failed rename: %v", err)
				}
				err = w.orphans(ex) // the next start
			}
			want := string(cut.Attempt) + fmt.Sprintf(c.detail, fmt.Sprintf("attempt %s is sealed; its publication is closed", cut.Attempt))
			if got := w.refusals(0); err != nil || !slices.Equal(got, []string{want}) {
				t.Fatalf("%v: refused %q; want %q", err, got, want)
			}
			empty(t, ex)
			w.settled(cut.Lease)
		})
	}
}
