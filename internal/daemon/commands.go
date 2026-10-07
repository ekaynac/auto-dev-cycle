// SPDX-License-Identifier: Apache-2.0

package daemon

// POST /v1/commands/{action} (08 "Reads, commands, events"): the bearer is
// authenticated on the listener that admitted the connection, the command
// is built from that principal (never from the request), submitted through
// the writer with the action's logic, and its committed disposition is
// answered as a CommandResponse. Every decided command is HTTP 200; a
// refusal names its registered reason.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// CommandsPath is the command endpoint's route pattern (apidoc.CommandPath).
const CommandsPath = "/v1/commands/{action}"

// commands serves CommandsPath over store, the listener's bearer
// verifications held to gate; a fault no refusal names goes to log, with
// its detail, and the client gets a fixed text.
type commands struct {
	store *eventstore.Store
	gate  *api.Slots
	reg   *vocab.Registry
	logic map[string]logic
	log   io.Writer
}

// authenticated, when set, runs after a request is authenticated and
// before its command is submitted: a test revokes the caller there.
var authenticated atomic.Pointer[func(identity.Principal)]

func (h commands) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		http.Error(w, "commands take no query parameters", http.StatusBadRequest)
		return
	}
	p, err := api.Authenticate(h.store, r, h.gate)
	switch {
	case errors.Is(err, api.ErrBusy):
		api.RefuseBusy(w)
		return
	case errors.Is(err, identity.ErrUnauthenticated):
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	case err != nil:
		h.fault(w, "authenticating", err)
		return
	}
	if f := authenticated.Load(); f != nil {
		(*f)(p)
	}
	raw, err := io.ReadAll(r.Body)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	req, derr := contract.Decode[types.CommandRequest]("api/command-request", raw)
	if errors.Join(err, derr) != nil {
		http.Error(w, "the request is not a valid CommandRequest", http.StatusBadRequest)
		return
	}
	v, err := h.decide(r.Context(), p, r.PathValue("action"), req)
	switch {
	case errors.Is(err, identity.ErrUnauthenticated): // revoked since it was authenticated
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	case errors.Is(err, authority.ErrInvalidScope):
		http.Error(w, "the scope is not valid for the action; nothing was decided", http.StatusBadRequest)
		return
	case errors.Is(err, eventstore.ErrClosed):
		v = refusal(vocab.CouldNotRun, vocab.ReasonStoreUnreadable, "the event store is closed")
	case err != nil:
		h.fault(w, r.PathValue("action"), err)
		return
	}
	out, err := contract.Encode("api/command-response", types.CommandResponse{Variant: v})
	if err != nil {
		h.fault(w, "encoding the response", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out) // the commit stands either way
}

// fault logs err with what was being done and answers 503 with a fixed
// text: nothing the client sent or the store holds is echoed.
func (h commands) fault(w http.ResponseWriter, what string, err error) {
	fmt.Fprintf(h.log, "adcd: command %s: %v\n", what, err)
	http.Error(w, "the command could not be decided; the controller log has the detail", http.StatusServiceUnavailable)
}

// decide submits the command and maps its committed disposition, or the
// refusal that kept it from committing.
func (h commands) decide(ctx context.Context, p identity.Principal, action string, req types.CommandRequest) (types.CommandResponseVariant, error) {
	if a, ok := h.reg.Actions[action]; !ok || a.Deprecated != nil {
		return refusal(vocab.Failed, vocab.ReasonActionUnregistered, "the action is not in the action registry"), nil
	}
	run, ok := h.logic[action]
	if !ok {
		return refusal(vocab.CouldNotRun, vocab.ReasonActionDeferred, action+" is not wired in this build"), nil
	}
	id, err := ids.New("cmd")
	if err != nil {
		return nil, err
	}
	c := p.Command(id, action, req.IdempotencyKey, req.Scope, nil)
	c.Expect = &eventstore.State{Version: req.Expected.Version, Digest: (*string)(req.Expected.Digest)}
	// No wired action issues a credential yet. One that does mints it here,
	// before Submit and outside the transaction, and passes it to applied
	// through identity.Issued.Deliver (14 N3).
	res, err := h.store.Submit(ctx, c, run(c))
	return h.answer(res, err)
}

// answer maps a submission: its committed disposition, or the refusal
// that kept it from committing; any other error is returned.
func (h commands) answer(res eventstore.Committed, err error) (types.CommandResponseVariant, error) {
	var ref *authority.Refusal
	switch {
	case errors.Is(err, eventstore.ErrIdempotencyKeyReused):
		return refusal(vocab.Failed, vocab.ReasonIdempotencyKeyReused, "the idempotency key was used for another request"), nil
	case errors.Is(err, eventstore.ErrStaleExpectedState):
		return refusal(vocab.Failed, vocab.ReasonStaleExpectedState, "the object is not at the expected state"), nil
	case errors.As(err, &ref):
		return refusal(vocab.Failed, ref.Reason, ref.Detail), nil
	case errors.Is(err, authority.ErrActionNotWired):
		return refusal(vocab.CouldNotRun, vocab.ReasonActionDeferred, "the command's operation is not wired in this build"), nil
	case err != nil:
		return nil, err
	}
	switch res.Disposition {
	case eventstore.DispositionApplied:
		return applied(res, nil)
	case eventstore.DispositionProposed:
		var v types.CommandProposed
		err := json.Unmarshal(res.Result, &v)
		v.Disposition = "proposed"
		return &v, err
	}
	// Refused and invalidated: the reason the result records (the
	// invalidation event stays in the log).
	var v types.CommandRefused
	err = json.Unmarshal(res.Result, &v)
	v.Disposition = "refused"
	return &v, errors.Join(err, h.reg.Check(vocab.Result{Outcome: vocab.Outcome(v.Outcome), Reason: v.Reason, Detail: v.Detail}))
}

// applied is the response of an applied command, with credential only when
// it was delivered fresh (never on a replay).
func applied(res eventstore.Committed, credential *identity.Secret) (types.CommandResponseVariant, error) {
	v := &types.CommandApplied{Disposition: "applied", Command: types.CommandID(res.CommandID), Replayed: res.Replayed,
		FirstSeq: res.FirstSeq, LastSeq: res.LastSeq, Effects: []types.CommandAppliedEffectsItem{}, Result: res.Result}
	var r struct {
		Approval *types.ApprovalID `json:"approval"`
	}
	if err := json.Unmarshal(res.Result, &r); err != nil {
		return nil, err
	}
	v.Approval = r.Approval
	for _, e := range res.Effects {
		v.Effects = append(v.Effects, types.CommandAppliedEffectsItem{ID: types.EffectID(e.ID), State: types.CommandAppliedEffectsItemState(e.State)})
	}
	if credential != nil {
		s := credential.Reveal()
		v.IssuedCredential = &s
	}
	return v, nil
}

func refusal(o vocab.Outcome, reason vocab.Reason, detail string) *types.CommandRefused {
	return &types.CommandRefused{Disposition: "refused", Outcome: types.CommandRefusedOutcome(o), Reason: reason, Detail: detail}
}
