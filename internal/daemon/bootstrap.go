// SPDX-License-Identifier: Apache-2.0

package daemon

// The one-time bootstrap (04 "Bootstrap", 14 R4). The first start of a
// store without an operator writes a fresh setup secret to
// <state_dir>/bootstrap.secret and records only its hash and its expiry,
// fixed then. POST /v1/bootstrap, on the private socket only, spends it:
// one writer transaction checks it and creates the first operator and its
// first token; the file is deleted after the commit.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The secret's file, and the system actor's own actions, outside the
// action registry: no bearer can send them (authorizeSystem).
const (
	bootstrapFile   = identity.BootstrapFile
	bootstrapAction = "identity.bootstrap"
	issueAction     = "identity.bootstrap_issue"
	renewAction     = "identity.bootstrap_renew" // adcd bootstrap renew (14 R8)
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
// (a crash after the commit) is removed. Unless issue (an offline
// command), it only holds the state directory.
func openBootstrap(ctx context.Context, store *eventstore.Store, system identity.Principal, stateDir string, now func() time.Time, issue bool) (_ *bootstrapper, note string, err error) {
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
	if !issue {
		return b, "", nil
	}
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
		if err := b.issue(ctx, false); err != nil {
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
// a link, and synced; then records its hash and expiry, as the first
// secret or, when renew, as a renewal (14 R8) whose fresh secret replaces
// the old one, never reusing it. A file without its event (a crash in
// between) never verifies, so it is replaced.
func (b *bootstrapper) issue(ctx context.Context, renew bool) error {
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
	record, action := identity.IssueBootstrap, issueAction
	if renew {
		record, action = identity.RenewBootstrap, renewAction
	}
	evs, err := record(b.system.Actor(), hash, b.now())
	if err != nil {
		return err
	}
	_, err = b.store.SubmitApplied(ctx, b.system.Command(cmd, action, cmd, []byte(`{}`), nil),
		eventstore.Applies(func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			return evs, json.RawMessage(`{}`), nil
		}))
	return err
}

// bootstrapResult is the stored result: ids only, never the secret.
type bootstrapResult struct {
	Operator types.ActorID `json:"operator"`
	Token    types.TokenID `json:"token"`
}

// The refusal of a wrong, expired, missing or spent secret, one for all.
var spentRefusal = types.BootstrapRefused{Disposition: "refused", Outcome: "failed", Reason: vocab.ReasonConfirmationRefused,
	Detail: "the bootstrap secret is wrong, expired or already used"}

// handler serves POST /v1/bootstrap with gate's verification slots. The
// listener that admitted the connection decides where it is served, never
// a header.
func (b *bootstrapper) handler(gate *api.Slots) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		peer, ok := api.PeerOf(r.Context())
		if !ok || peer.Via != identity.PrivateSocket {
			http.Error(w, "bootstrap is served only on the private socket", http.StatusForbidden)
			return
		}
		release, ok := gate.Take(peer.UID)
		if !ok {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many authentications in progress; retry", http.StatusServiceUnavailable)
			return
		}
		defer release()
		raw, err := io.ReadAll(r.Body)
		req, derr := contract.Decode[types.BootstrapRequest]("api/bootstrap-request", raw)
		secret, perr := identity.ParseBootstrapSecret(req.Secret)
		if errors.Join(err, derr, perr) != nil {
			http.Error(w, "the bootstrap request is not a valid BootstrapRequest", http.StatusBadRequest)
			return
		}
		status, v := b.consume(r.Context(), req, secret)
		out, err := contract.Encode("api/bootstrap-response", types.BootstrapResponse{Variant: v})
		if err != nil {
			http.Error(w, "the bootstrap response cannot be encoded", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(out) // the commit stands either way
	}
}

// consume submits the bootstrap as the system actor. The token is minted
// before, outside the transaction, and goes to the response only from the
// commit that applied it (14 N3). The input binds the secret's digest, so
// the same key with another secret is another request. Expiry is judged
// by the clock inside the transaction, never by when the request arrived
// (minting and the writer queue take time).
func (b *bootstrapper) consume(ctx context.Context, req types.BootstrapRequest, secret identity.Secret) (int, types.BootstrapResponseVariant) {
	now := b.now() // the token's created_at only
	op, err := ids.New("act")
	cmd, cerr := ids.New("cmd")
	issued, merr := identity.Mint(types.ActorID(op), b.system.Actor(), now)
	sum := sha256.Sum256([]byte(secret.Reveal()))
	input, ierr := json.Marshal(map[string]string{"name": req.Name, "secret_sha256": hex.EncodeToString(sum[:])})
	var c eventstore.Committed
	if err = errors.Join(err, cerr, merr, ierr); err == nil {
		actor := types.Actor{ID: types.ActorID(op), Kind: types.ActorKindOperator, Name: req.Name, CreatedBy: b.system.Actor()}
		c, err = b.store.Submit(ctx, b.system.Command(cmd, bootstrapAction, "bootstrap/"+req.IdempotencyKey, input, nil),
			func(t *eventstore.Tx) (eventstore.Decision, error) {
				evs, err := identity.ConsumeBootstrap(t, secret, actor, issued, b.now())
				if errors.Is(err, identity.ErrBootstrapRefused) {
					res, err := json.Marshal(spentRefusal)
					return eventstore.Decision{Disposition: eventstore.DispositionRefused, Result: res}, err
				}
				res, merr := json.Marshal(bootstrapResult{actor.ID, issued.Token.ID})
				return eventstore.Decision{Disposition: eventstore.DispositionApplied, Events: evs, Result: res}, errors.Join(err, merr)
			})
	}
	switch {
	case errors.Is(err, eventstore.ErrIdempotencyKeyReused):
		return http.StatusConflict, &types.BootstrapRefused{Disposition: "refused", Outcome: "failed", Reason: vocab.ReasonIdempotencyKeyReused,
			Detail: "the idempotency key was used for another bootstrap request"}
	case err != nil:
		return http.StatusServiceUnavailable, &types.BootstrapRefused{Disposition: "refused", Outcome: "could_not_run", Reason: vocab.ReasonStoreUnreadable,
			Detail: "the bootstrap could not be recorded"}
	case c.Disposition != eventstore.DispositionApplied:
		var r types.BootstrapRefused
		if json.Unmarshal(c.Result, &r) != nil {
			r = spentRefusal
		}
		return http.StatusOK, &r
	}
	// The commit came first: a file left by a failed removal is spent, and
	// the next start removes it.
	_ = b.remove()
	var res bootstrapResult
	_ = json.Unmarshal(c.Result, &res) // the store's own canonical result
	out := &types.BootstrapApplied{Disposition: "applied", Command: types.CommandID(c.CommandID), Replayed: c.Replayed, Operator: res.Operator, Token: res.Token}
	if s, ok := issued.Deliver(c); ok {
		v := s.Reveal()
		out.IssuedCredential = &v
	}
	return http.StatusOK, out
}
