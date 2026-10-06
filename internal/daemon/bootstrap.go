// SPDX-License-Identifier: Apache-2.0

package daemon

// The one-time bootstrap (04 "Bootstrap", 14 R4). The first start of a
// store without an operator writes a fresh setup secret to
// <state_dir>/bootstrap.secret and records only its hash and its expiry,
// fixed then. (Spending it, POST /v1/bootstrap, comes next.)

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The secret's file, and the system actor's own actions, outside the
// action registry: no bearer can send them (authorizeSystem).
const (
	bootstrapFile = identity.BootstrapFile
	issueAction   = "identity.bootstrap_issue"
)

// The fixed-text startup notes; none names a path or the secret.
const (
	notePending = "adcd: bootstrap pending; the one-time setup secret is bootstrap.secret in the state directory (adc admin bootstrap)"
	noteMissing = "adcd: bootstrap pending, but bootstrap.secret is missing; it is not recreated: run adcd bootstrap renew"
	noteExpired = "adcd: the bootstrap secret expired unused; run adcd bootstrap renew"
)

// authorizeSystem admits a system action for the system principal only.
func authorizeSystem(t *eventstore.Tx, c eventstore.Command) error {
	a, err := identity.ActorOf(t, c)
	if err == nil && a.Kind != types.ActorKindSystem {
		err = identity.ErrUnauthenticated
	}
	return err
}

// bootstrapper holds what the bootstrap needs: the state directory, held
// open, so the file is made and removed through it, never by a path.
type bootstrapper struct {
	store  *eventstore.Store
	system identity.Principal
	dir    *os.File
	now    func() time.Time
}

// openBootstrap runs at startup: with no operator and no secret recorded
// it issues one; otherwise it only says what is pending. A restart never
// renews or recreates a secret, and a consumed bootstrap's leftover file
// (a crash after the commit) is removed.
func openBootstrap(ctx context.Context, store *eventstore.Store, system identity.Principal, stateDir string, now func() time.Time) (_ *bootstrapper, note string, err error) {
	dir, err := openDir(stateDir, func(st unix.Stat_t) bool { return st.Mode&0o777 == 0o700 })
	if errors.Is(err, errUnknownFS) {
		return nil, "", configError(fsRefusal)
	}
	if err != nil {
		return nil, "", configError("the state directory must be a directory owned by this user with mode 0700")
	}
	b := &bootstrapper{store: store, system: system, dir: dir, now: now}
	defer func() {
		if err != nil {
			dir.Close()
		}
	}()
	var st identity.BootstrapState
	if err := store.View(ctx, func(tx *eventstore.Tx) (err error) {
		st, err = identity.BootstrapOf(tx)
		return err
	}); err != nil {
		return nil, "", couldNotRun(vocab.ReasonStoreUnreadable, "the bootstrap state cannot be read", err)
	}
	var fst unix.Stat_t
	present := unix.Fstatat(int(dir.Fd()), bootstrapFile, &fst, unix.AT_SYMLINK_NOFOLLOW) == nil
	switch {
	case st.Consumed:
		if present && b.remove() != nil {
			return nil, "", couldNotRun(vocab.ReasonStoreUnreadable, "the spent bootstrap secret cannot be removed", nil)
		}
		return b, "", nil
	case st.Secret == nil:
		if err := b.issue(ctx); err != nil {
			return nil, "", couldNotRun(vocab.ReasonStoreUnreadable, "the bootstrap secret cannot be issued", err)
		}
		return b, notePending, nil
	case !st.Live(now()):
		return b, noteExpired, nil
	case !present:
		return b, noteMissing, nil
	}
	return b, notePending, nil
}

func (b *bootstrapper) remove() error {
	err := unix.Unlinkat(int(b.dir.Fd()), bootstrapFile, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

// issue writes a fresh secret, created exclusively 0600 without following
// a link, and synced; then records its hash and expiry. A file without
// its event (a crash in between) never verifies, so it is replaced.
func (b *bootstrapper) issue(ctx context.Context) error {
	secret, hash, err := identity.NewBootstrapSecret()
	if err != nil {
		return err
	}
	if err := b.remove(); err != nil {
		return err
	}
	fd, err := unix.Openat(int(b.dir.Fd()), bootstrapFile, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), bootstrapFile)
	_, werr := io.WriteString(f, secret.Reveal()+"\n")
	if err := errors.Join(werr, f.Sync(), f.Close(), b.dir.Sync()); err != nil {
		return err
	}
	cmd, err := ids.New("cmd")
	if err != nil {
		return err
	}
	evs, err := identity.IssueBootstrap(b.system.Actor(), hash, b.now())
	if err != nil {
		return err
	}
	_, err = b.store.SubmitApplied(ctx, b.system.Command(cmd, issueAction, cmd, []byte(`{}`), nil),
		eventstore.Applies(func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			return evs, json.RawMessage(`{}`), nil
		}))
	return err
}
