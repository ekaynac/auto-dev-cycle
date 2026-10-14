// SPDX-License-Identifier: Apache-2.0

//go:build linux

package recovery

// Orphaned output (X3 "Late output"; 09). RefuseOrphans runs at start, after Run, before any launch; Run never waits
// for it (H1): step 1 fenced the cut attempts, so a refusal is a record, not a precondition. A root is bound through
// the store, never a name read: for each attempt lost{controller_restart} (perhaps sealed or superseded since),
// exactly its adapterhost.ExchangeName, opened O_PATH|O_NOFOLLOW, if a directory of the controller's euid at mode
// 0700. Each entry of its out/ is refused (stale_attempt) by lstat size beneath out/'s descriptor, digest of no bytes,
// never opened; an out/ not listed whole (a symlink, mode 000, past maxEntries) adds a refusal "out" saying why. The
// idempotency key is the root's name: a crash before the rename to refused-<name> replays it. At most maxRoots roots a
// start. Any other exchange-* entry is removed with no event, and refused-* swept (adapterhost.RemoveAt).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

const (
	ActionRefuse = "recovery.refuse_output"

	refusedPrefix = "refused-"
	maxRoots      = 16
	maxEntries    = 64
	emptyDigest   = types.Digest("sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
)

// RefuseOrphans refuses the output each attempt a restart cut left in its exchange root, then clears the exchange
// directory of every root; the error is every failure, none stopping the others.
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
	claimed, roots := map[string]bool{}, 0
	for _, id := range cut {
		name := adapterhost.ExchangeName(id)
		claimed[name] = true
		if roots < maxRoots {
			found, rerr := r.refuseRoot(ctx, dir, name, id)
			if found {
				roots++
			}
			err = errors.Join(err, rerr)
		}
	}
	names, lerr := parent.Readdirnames(-1)
	err = errors.Join(err, lerr)
	for _, n := range names {
		if strings.HasPrefix(n, refusedPrefix) || strings.HasPrefix(n, adapterhost.ExchangePrefix) && !claimed[n] {
			err = errors.Join(err, adapterhost.RemoveAt(dir, n))
		}
	}
	return err
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

// refuseRoot refuses the output in name, attempt id's root beneath dir, then renames it; found: name exists.
func (r Recovery) refuseRoot(ctx context.Context, dir int, name string, id types.AttemptID) (found bool, err error) {
	fd, err := unix.Openat(dir, name, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == unix.ENOENT {
		return false, nil
	}
	var st unix.Stat_t
	if err == nil {
		defer unix.Close(fd)
		err = unix.Fstat(fd, &st)
	}
	if err != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&0o7777 != 0o700 {
		return true, adapterhost.RemoveAt(dir, name) // not a root the controller made: no event
	}
	if handles, note := listOut(fd, id); len(handles) > 0 {
		err = r.submit(ctx, ActionRefuse, ActionRefuse+"/"+name, &id, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			var stale *lease.Stale
			a, _, _, err := lease.AttemptAt(tx, id)
			if err == nil {
				err = lease.FenceAttempt(tx, id, a.FencingToken)
			}
			if !errors.As(err, &stale) { // never for a cut attempt: its publication closed at step 1
				return nil, errors.Join(err, fmt.Errorf("recovery: attempt %s may still publish", id))
			}
			evs, errs := make([]eventstore.Event, len(handles)), make([]error, len(handles))
			for i, h := range handles {
				detail := stale.Detail + "; not read"
				if i == len(handles)-1 {
					detail += note
				}
				evs[i], _, errs[i] = lease.Refusal(id, h, types.AttemptOutputRefusalCheckStaleAttempt, detail)
			}
			return evs, errors.Join(errs...)
		})
	}
	if err == nil {
		err = unix.Renameat(dir, name, dir, refusedPrefix+name)
	}
	if err != nil {
		return true, fmt.Errorf("recovery: the orphaned output of %s: %w", id, err)
	}
	return true, nil
}

// listOut names out/'s entries beneath root, at most maxEntries, by lstat beneath out/'s descriptor, opening none.
// When out/ was not listed whole, a last handle "out" stands for the rest, and note says why.
func listOut(root int, id types.AttemptID) (found []types.AttemptOutputRefusalHandle, note string) {
	handle := func(name string, size int64) types.AttemptOutputRefusalHandle {
		return types.AttemptOutputRefusalHandle{Attempt: id, Handle: name, Digest: emptyDigest, Size: types.Amount(max(size, 0))}
	}
	fd, err := unix.Openat(root, "out", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == unix.ENOENT {
		return nil, ""
	}
	var names []string
	if err == nil {
		out := os.NewFile(uintptr(fd), "out")
		defer out.Close()
		if names, err = out.Readdirnames(maxEntries + 1); errors.Is(err, io.EOF) {
			err = nil
		}
	}
	for _, n := range names[:min(len(names), maxEntries)] {
		var st unix.Stat_t // an entry gone meanwhile is still refused, by name, at size 0
		_ = unix.Fstatat(fd, n, &st, unix.AT_SYMLINK_NOFOLLOW)
		found = append(found, handle(n, st.Size))
	}
	var errno unix.Errno
	switch {
	case errors.As(err, &errno):
		note = "; out/ not listed: " + unix.ErrnoName(errno)
	case err != nil:
		note = "; out/ not listed"
	case len(names) > maxEntries:
		note = fmt.Sprintf("; out/ truncated at %d entries", maxEntries)
	default:
		return found, ""
	}
	return append(found, handle("out", 0)), note
}
