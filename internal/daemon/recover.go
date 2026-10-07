// SPDX-License-Identifier: Apache-2.0

package daemon

// `adcd recover operator-token` (14 R7): an offline command (offline.go)
// that issues an existing live operator a new token, written only to the
// file the caller names.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const recoverUsage = "usage: adcd recover operator-token --state-dir DIR --actor ID --out FILE [--revoke-others] [--force-overwrite] [--resource-groups FILE] [--trusted-head SEQ:sha256:HEX]"

var errNotOperator = &StartError{Outcome: vocab.Failed, Reason: vocab.ReasonActorRevoked, Detail: "the target is not a live operator (missing, revoked or another kind)"}

// RecoverCommand is `adcd recover operator-token`.
func RecoverCommand(args []string, stderr io.Writer) int {
	var actor, out *string
	var revoke, force *bool
	cfg, err := loadConfig("adcd recover operator-token", recoverUsage, args, func(fs *flag.FlagSet) func(*Config) {
		actor, out = fs.String("actor", "", ""), fs.String("out", "", "")
		revoke, force = fs.Bool("revoke-others", false, ""), fs.Bool("force-overwrite", false, "")
		return noFlags(fs)
	})
	if err == nil && (*actor == "" || *out == "") {
		err = configError("--actor and --out are required; " + recoverUsage)
	}
	if err == nil {
		err = recoverToken(context.Background(), cfg, types.ActorID(*actor), outFile{*out, *force}, *revoke)
	}
	if err == nil {
		fmt.Fprintln(stderr, "adcd: token recovered; the operator's new token is only in the --out file")
	}
	return exitCode(err, stderr)
}

// recoverToken mints target's token outside the transaction, commits it
// (revoking target's other live tokens when revokeOthers) and writes the
// secret to out only. It confirms nothing and resurrects no one.
func recoverToken(ctx context.Context, cfg Config, target types.ActorID, out outFile, revokeOthers bool) error {
	dir, err := out.open() // refused before the store is touched
	if err != nil {
		return err
	}
	defer dir.Close()
	return offline(ctx, cfg, func(d *Daemon) error {
		issued, err := identity.Mint(target, d.system.Actor(), time.Now())
		cmd, cerr := ids.New("cmd")
		input, ierr := json.Marshal(map[string]any{"actor": target, "revoke_others": revokeOthers})
		if err := errors.Join(err, cerr, ierr); err != nil {
			return couldNotRun(vocab.ReasonStoreUnreadable, "the token cannot be minted", err)
		}
		c, err := d.store.Submit(ctx, d.system.Command(cmd, recoverAction, cmd, input, nil), func(tx *eventstore.Tx) (eventstore.Decision, error) {
			others, err := liveTokens(tx, target)
			if !revokeOthers {
				others = nil
			}
			evs, eerr := identity.RecoverToken(issued, others)
			res, merr := json.Marshal(map[string]any{"token": issued.Token.ID, "revoked": len(others)})
			return eventstore.Decision{Disposition: eventstore.DispositionApplied, Events: evs, Result: res}, errors.Join(err, eerr, merr)
		})
		if errors.Is(err, errNotOperator) {
			return errNotOperator
		} else if err != nil {
			return couldNotRun(vocab.ReasonStoreUnreadable, "the recovery could not be recorded", err)
		}
		secret, ok := issued.Deliver(c)
		if !ok {
			return couldNotRun(vocab.ReasonAmbiguousState, "the recovery was not applied", nil)
		}
		if err := out.write(dir, secret); err != nil {
			return couldNotRun(vocab.ReasonStoreUnreadable, "the token is recorded but the --out file could not be written; recover again with --revoke-others", err)
		}
		return nil
	})
}

// liveTokens refuses a target that is not a live operator, and returns its
// live tokens.
func liveTokens(tx *eventstore.Tx, target types.ActorID) ([]types.TokenID, error) {
	var a types.Actor
	raw, ok, err := tx.Get(identity.ActorProjection, string(target))
	if err == nil && ok {
		err = json.Unmarshal(raw, &a)
	}
	if err != nil {
		return nil, err
	}
	if !ok || a.Kind != types.ActorKindOperator || a.RevokedAt != nil {
		return nil, errNotOperator
	}
	keys, err := tx.Keys(identity.TokenProjection, "")
	var live []types.TokenID
	for _, k := range keys {
		var t types.Token
		raw, _, gerr := tx.Get(identity.TokenProjection, k)
		if err = errors.Join(err, gerr, json.Unmarshal(raw, &t)); err != nil {
			return nil, err
		}
		if t.Actor == target && t.RevokedAt == nil {
			live = append(live, t.ID)
		}
	}
	return live, err
}

// outFile is --out, written as the token file is (R5): in a directory of
// this user's that no group or other can write, never through a link.
type outFile struct {
	path  string
	force bool // replace an existing --out
}

// open opens --out's directory, checked on its descriptor, and refuses an
// existing --out unless force.
func (o outFile) open() (*os.File, error) {
	abs, err := filepath.Abs(o.path)
	if err != nil {
		return nil, configError("--out cannot be resolved")
	}
	dir, err := openDir(filepath.Dir(abs), func(st unix.Stat_t) bool { return st.Mode&0o022 == 0 })
	if err != nil {
		return nil, configError("the --out directory must be this user's, not writable by group or others (a symlink is refused)")
	}
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), filepath.Base(abs), &st, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) && !o.force {
		dir.Close()
		return nil, configError("--out exists; --force-overwrite replaces it")
	}
	return dir, nil
}

// write creates a temporary file exclusively, 0600, without following a
// link, writes and syncs the secret, then renames it to --out (never over
// one unless force) and syncs the directory.
func (o outFile) write(dir *os.File, s identity.Secret) error {
	abs, err := filepath.Abs(o.path)
	if err != nil {
		return err
	}
	dfd, tmp := int(dir.Fd()), ".adcd-token-"+rand.Text()
	fd, err := unix.Openat(dfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), tmp)
	cherr := f.Chmod(0o600)
	_, werr := io.WriteString(f, s.Reveal()+"\n")
	err = errors.Join(cherr, werr, f.Sync(), f.Close())
	flags := uint(unix.RENAME_NOREPLACE)
	if o.force {
		flags = 0
	}
	if err == nil {
		err = unix.Renameat2(dfd, tmp, dfd, filepath.Base(abs), flags)
	}
	if err != nil {
		_ = unix.Unlinkat(dfd, tmp, 0)
		return err
	}
	return dir.Sync()
}
