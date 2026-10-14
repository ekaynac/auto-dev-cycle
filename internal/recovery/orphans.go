// SPDX-License-Identifier: Apache-2.0

//go:build linux

package recovery

// Orphaned output (X3 "Late output"; 09). RefuseOrphans runs at start, after Run, before any launch; Run never waits
// for it (H1). A root is bound through the store: for each attempt lost{controller_restart}, exactly its
// adapterhost.ExchangeName, opened O_PATH|O_NOFOLLOW, if the controller's 0700 directory. Each out/ entry is refused
// (stale_attempt) by lstat size (clamped) beneath out/'s descriptor, never opened; an out/ not listed whole adds a
// refusal "out/" (no entry's name) saying why. Only a *contract.Error falls back to one summary refusal; any other
// failure keeps the root for the next start. The key is the root's name, so a crash before the rename replays it. At
// most maxRoots roots' refusals are newly recorded a start. Every other exchange-* entry is removed with no event and
// refused-* swept, within one adapterhost.Cleanup budget.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

const (
	ActionRefuse = "recovery.refuse_output"

	maxEntries  = 64
	dirHandle   = "out/" // out/ itself: no entry of it has a "/" in its name
	emptyDigest = types.Digest("sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
)

// Variables for tests (export_test.go): the roots whose refusals one start records, the largest size recorded
// (Amount's maximum), the rename of a refused root, and the refusal command.
var (
	maxRoots            = 16
	maxSize       int64 = 1<<53 - 1
	renameat            = unix.Renameat
	submitRefusal       = Recovery.submit
)

// RefuseOrphans refuses the output each attempt a restart cut left in its exchange root, then clears the exchange
// directory of every root, within adapterhost.MaxWork entries; the error is every failure, none stopping the others.
func (r Recovery) RefuseOrphans(ctx context.Context) error {
	if r.Exchanges == "" {
		return nil
	}
	cut, err := r.cutAttempts(ctx)
	if err != nil {
		return err
	}
	dir, err := unix.Open(r.Exchanges, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err == unix.ENOENT {
		return nil // no root was ever made
	} else if err != nil {
		return fmt.Errorf("recovery: the exchange directory: %w", err)
	}
	parent := os.NewFile(uintptr(dir), r.Exchanges)
	defer parent.Close()
	c := &adapterhost.Cleanup{Ctx: ctx, Left: adapterhost.MaxWork}
	claimed, records := map[string]bool{}, 0
	for _, id := range cut {
		name := adapterhost.ExchangeName(id)
		claimed[name] = true
		if records < maxRoots {
			recorded, rerr := r.refuseRoot(c, dir, name, id)
			if recorded {
				records++
			}
			err = cmp.Or(err, rerr) // the first failure only: the diagnostics stay bounded
		}
	}
	for sweep := true; sweep; {
		names, lerr := parent.Readdirnames(256)
		for _, n := range names {
			if sweep = c.Visit(); !sweep {
				break
			}
			if strings.HasPrefix(n, adapterhost.RefusedPrefix) || strings.HasPrefix(n, adapterhost.ExchangePrefix) && !claimed[n] {
				c.RemoveAt(dir, n)
			}
		}
		if lerr != nil && lerr != io.EOF {
			err = cmp.Or(err, lerr)
		}
		sweep = sweep && lerr == nil
	}
	return errors.Join(err, c.Err())
}

// cutAttempts is every attempt a restart cut, in key order. A running attempt means step 1 has not run: then nothing
// is touched.
func (r Recovery) cutAttempts(ctx context.Context) (cut []types.AttemptID, err error) {
	err = r.Store.View(ctx, func(tx *eventstore.Tx) error {
		keys, err := tx.Keys(lease.AttemptProjection, "")
		for _, k := range keys {
			a, _, _, aerr := lease.AttemptAt(tx, types.AttemptID(k))
			switch {
			case aerr != nil:
				return aerr
			case a.Status == types.AttemptStatusRunning:
				return fmt.Errorf("recovery: attempt %s is running: orphaned output is refused after step 1", k)
			case a.LostReason != nil && *a.LostReason == restart:
				cut = append(cut, a.ID)
			}
		}
		return err
	})
	return cut, err
}

// refuseRoot refuses the output in name, attempt id's root beneath dir, then renames it; recorded: its refusals were
// newly committed (not replayed).
func (r Recovery) refuseRoot(c *adapterhost.Cleanup, dir int, name string, id types.AttemptID) (recorded bool, err error) {
	fd, err := unix.Openat(dir, name, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == nil {
		defer unix.Close(fd)
	}
	if err == unix.ENOENT || !c.Visit() {
		return false, nil // gone; or the work budget is spent, and the root waits
	}
	var st unix.Stat_t
	if err == nil {
		err = unix.Fstat(fd, &st)
	}
	if err != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&0o7777 != 0o700 {
		c.RemoveAt(dir, name) // not a root the controller made: no event
		return false, nil
	}
	refuse := func(orphans []orphan) (bool, error) {
		decided := false // a replay decides nothing
		err := submitRefusal(r, c.Ctx, ActionRefuse, ActionRefuse+"/"+name, &id, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			var stale *lease.Stale
			a, _, _, err := lease.AttemptAt(tx, id)
			if err == nil {
				err = lease.FenceAttempt(tx, id, a.FencingToken)
			}
			if !errors.As(err, &stale) { // never for a cut attempt: its publication closed at step 1
				return nil, errors.Join(err, fmt.Errorf("recovery: attempt %s may still publish", id))
			}
			evs, errs := make([]eventstore.Event, len(orphans)), make([]error, len(orphans))
			for i, o := range orphans {
				evs[i], _, errs[i] = lease.Refusal(id, o.handle, types.AttemptOutputRefusalCheckStaleAttempt, stale.Detail+"; not read"+o.note)
			}
			decided = true
			return evs, errors.Join(errs...)
		})
		return decided && err == nil, err
	}
	if orphans := listOut(fd, id); len(orphans) > 0 {
		var invalid *contract.Error // the contract refuses a record deterministically: a summary takes its place
		if recorded, err = refuse(orphans); errors.As(err, &invalid) {
			recorded, err = refuse([]orphan{entry(id, dirHandle, 0, "; out/ not recorded entry by entry")})
		}
	}
	if err == nil && !c.RemoveAt(dir, adapterhost.RefusedPrefix+name) { // a leftover never blocks the rename
		err = errors.New("an earlier refused root stays")
	}
	if err == nil {
		err = renameat(dir, name, dir, adapterhost.RefusedPrefix+name)
	}
	if err != nil {
		return recorded, fmt.Errorf("recovery: the orphaned output of %s: %w", id, err)
	}
	return recorded, nil
}

// orphan is one refused output, with what its refusal's detail adds.
type orphan struct {
	handle types.AttemptOutputRefusalHandle
	note   string
}

func entry(id types.AttemptID, name string, size int64, note string) orphan {
	if size > maxSize {
		size, note = maxSize, note+fmt.Sprintf("; lstat size %d, recorded as %d", size, maxSize)
	}
	return orphan{types.AttemptOutputRefusalHandle{Attempt: id, Handle: name, Digest: emptyDigest, Size: types.Amount(max(size, 0))}, note}
}

// listOut names out/'s entries beneath root, at most maxEntries, by lstat beneath out/'s descriptor, opening none.
// When out/ was not listed whole, a last orphan "out/" says why.
func listOut(root int, id types.AttemptID) (found []orphan) {
	fd, err := unix.Openat(root, "out", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == unix.ENOENT {
		return nil
	}
	var names []string
	if err == nil {
		out := os.NewFile(uintptr(fd), "out")
		defer out.Close()
		if names, err = out.Readdirnames(maxEntries + 1); err == io.EOF {
			err = nil
		}
	}
	for _, n := range names[:min(len(names), maxEntries)] {
		var st unix.Stat_t // an entry gone meanwhile is still refused, by name, at size 0
		_ = unix.Fstatat(fd, n, &st, unix.AT_SYMLINK_NOFOLLOW)
		found = append(found, entry(id, n, st.Size, ""))
	}
	var errno unix.Errno
	switch {
	case errors.As(err, &errno):
		return append(found, entry(id, dirHandle, 0, "; out/ not listed: "+unix.ErrnoName(errno)))
	case err != nil:
		return append(found, entry(id, dirHandle, 0, "; out/ not listed"))
	case len(names) > maxEntries:
		return append(found, entry(id, dirHandle, 0, fmt.Sprintf("; out/ truncated at %d entries", maxEntries)))
	}
	return found
}
