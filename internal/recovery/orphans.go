// SPDX-License-Identifier: Apache-2.0

//go:build linux

package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

// Orphaned output (X3 "Late output"). An exchange root outlives its run only when the controller died during it.
// Each entry of its out/ is refused by the fence (lease.FenceAttempt at the attempt's own token, which a closed
// publication or a replaced attempt fails), as attempt.output_refused{stale_attempt}. As 09 has it for a stale
// output, nothing is read: no entry is opened, followed or published; each is named by its lstat size and the digest
// of no bytes. At most maxOrphans per root. The root is then renamed out of the prefix and removed: a crash before
// the rename refuses it again at the next start, and a root left unremoved is reported once the steps ran.
const (
	maxOrphans  = 64
	emptyDigest = types.Digest("sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
)

// refuseOrphans refuses the output of every exchange root under r.Exchanges; cleanup is what removing them failed.
func (r Recovery) refuseOrphans(ctx context.Context) (cleanup, err error) {
	if r.Exchanges == "" {
		return nil, nil
	}
	roots, err := filepath.Glob(filepath.Join(r.Exchanges, adapterhost.ExchangePrefix+"*"))
	for _, root := range roots {
		name := strings.TrimPrefix(filepath.Base(root), adapterhost.ExchangePrefix)
		cut := strings.LastIndexByte(name, '-')
		if cut <= 0 || ids.Check(name[:cut], "att") != nil {
			continue // not a root adapterhost made
		}
		id := types.AttemptID(name[:cut])
		handles, err := orphans(filepath.Join(root, "out"), id)
		if err == nil && len(handles) > 0 {
			err = r.submit(ctx, ActionRefuse, &id, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				var stale *lease.Stale
				a, _, _, err := lease.AttemptAt(tx, id)
				if err == nil {
					err = lease.FenceAttempt(tx, id, a.FencingToken)
				}
				if !errors.As(err, &stale) {
					return nil, errors.Join(err, fmt.Errorf("recovery: attempt %s may still publish", id))
				}
				evs, errs := make([]eventstore.Event, len(handles)), make([]error, len(handles))
				for i, h := range handles {
					evs[i], _, errs[i] = lease.Refusal(id, h, types.AttemptOutputRefusalCheckStaleAttempt, stale.Detail+"; not read")
				}
				return evs, errors.Join(errs...)
			})
		}
		gone := filepath.Join(r.Exchanges, "refused-"+filepath.Base(root))
		if err == nil {
			err = os.Rename(root, gone)
		}
		if err != nil {
			return cleanup, fmt.Errorf("recovery: the orphaned output of %s: %w", id, err)
		}
		_ = os.Chmod(filepath.Join(gone, "in"), 0o755) // in/ is read-only by mode; RemoveAll reports what stays
		cleanup = errors.Join(cleanup, os.RemoveAll(gone))
	}
	return cleanup, err
}

// orphans lists out/'s entries, at most maxOrphans, by lstat beneath out/'s descriptor, opening none of them.
func orphans(out string, id types.AttemptID) ([]types.AttemptOutputRefusalHandle, error) {
	fd, err := unix.Open(out, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), out)
	defer dir.Close()
	names, err := dir.Readdirnames(maxOrphans)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	found := make([]types.AttemptOutputRefusalHandle, len(names))
	for i, n := range names {
		var st unix.Stat_t // an entry gone or not inspectable is still refused, by name, at size 0
		_ = unix.Fstatat(fd, n, &st, unix.AT_SYMLINK_NOFOLLOW)
		found[i] = types.AttemptOutputRefusalHandle{Attempt: id, Handle: n, Digest: emptyDigest, Size: types.Amount(max(st.Size, 0))}
	}
	return found, err
}
