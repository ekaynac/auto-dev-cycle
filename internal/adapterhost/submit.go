// SPDX-License-Identifier: Apache-2.0

//go:build linux

package adapterhost

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/ingest"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

// Host faults after launch, with fixed texts: the underlying errors can
// quote names the adapter chose. Classification survives (errors.Is
// ingest.ErrUnsupported, or a context error).
var (
	ErrIngest = errors.New("adapterhost: ingesting an adapter output failed")
	ErrRecord = errors.New("adapterhost: recording the attempt's end failed")
)

type hostError struct{ wraps []error }

func (e *hostError) Error() string   { return e.wraps[0].Error() }
func (e *hostError) Unwrap() []error { return e.wraps }

func hostFault(text, err error) error {
	h := &hostError{wraps: []error{text}}
	for _, class := range []error{ingest.ErrUnsupported, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, class) {
			h.wraps = append(h.wraps, class)
		}
	}
	return h
}

func newID(prefix string) (string, error) { return ids.New(prefix) }

// output is one final output to check, with its bound.
type output struct {
	h     types.AdapterHandle
	bound int64
}

// outputs are the run's final outputs: the handles its events declared and
// the result's, deduplicated.
func (r *attemptRun) outputs(result types.AdapterRunResult) []output {
	r.mu.Lock()
	defer r.mu.Unlock()
	var outs []output
	for _, h := range r.declared {
		outs = append(outs, output{h, BoundEventHandle})
	}
	if res := result.Result; res != nil {
		b := map[types.AdapterRunResultResultValueKind]int64{types.AdapterRunResultResultValueKindPatch: BoundPatch,
			types.AdapterRunResultResultValueKindText: BoundText}[res.Kind]
		if b == 0 {
			b = BoundDecision
		}
		for i, o := range outs {
			if o.h == res.Handle {
				outs = append(outs[:i], outs[i+1:]...) // the result's bound applies
				break
			}
		}
		outs = append(outs, output{res.Handle, b})
	}
	return outs
}

// admit checks every final output in 09's order, after the tree exited:
// the fence first (stale refuses them all and reads nothing), then for each
// the foreign handle and the file checks, copying each verified output into
// the blob store. TODO(staging): the copy is published here, before the
// transaction's fence; a stacked change stages it instead.
func (r *attemptRun) admit(ctx context.Context, result types.AdapterRunResult) error {
	outs := r.outputs(result)
	if len(outs) == 0 {
		r.admitted = true
		return nil
	}
	stale, err := r.fence(ctx)
	if err != nil {
		return err
	}
	r.blobs = map[types.AdapterHandle]types.Digest{}
	for _, o := range outs {
		rh := refusalHandle(o.h)
		switch {
		case stale != nil:
			r.refuse(rh, types.AttemptOutputRefusalCheckStaleAttempt, stale.Detail)
			continue
		case o.h.Attempt != r.spec.Attempt:
			r.refuse(rh, types.AttemptOutputRefusalCheckForeignHandle, "a handle of another attempt than the submitting one")
			continue
		}
		d, err := ingestFile(r.x.outDir, o.h.Handle, string(o.h.Digest), int64(o.h.Size), o.bound, r.h.Blobs)
		var u *ingest.Unsafe
		switch {
		case errors.As(err, &u):
			r.refuse(rh, types.AttemptOutputRefusalCheck(u.Check), u.Detail)
		case err != nil:
			return hostFault(ErrIngest, err)
		case d == "":
			return hostFault(ErrIngest, errors.New("no digest"))
		default:
			r.blobs[o.h] = types.Digest(d)
		}
	}
	r.admitted = !r.denied && len(r.blobs) == len(outs)
	return nil
}

// refused classifies the output of a run the session refused: the result
// handle it declared (or the foreign handle of an event), by the fence,
// the foreign handle and the path check, in that order. Nothing is read.
func (r *attemptRun) refused(ctx context.Context, s *session.Session) error {
	var h *types.AttemptOutputRefusalHandle
	if d, ok := s.Declared(); ok {
		h, r.declaredFinal = d.Handle, d.UsageFinal
	}
	if f, ok := s.Foreign(); ok && h == nil {
		rh := refusalHandle(f)
		h = &rh
	}
	if h == nil {
		return nil
	}
	stale, err := r.fence(ctx)
	switch {
	case err != nil:
		return err
	case stale != nil:
		r.refuse(*h, types.AttemptOutputRefusalCheckStaleAttempt, stale.Detail)
	case h.Attempt != r.spec.Attempt:
		r.refuse(*h, types.AttemptOutputRefusalCheckForeignHandle, "a handle of another attempt than the submitting one")
	case !ingest.ValidHandle(h.Handle):
		r.refuse(*h, types.AttemptOutputRefusalCheckPath, "the handle is not one plain path component")
	}
	return nil
}

// fence is the first fence: a *lease.Stale when the attempt may no longer
// publish.
func (r *attemptRun) fence(ctx context.Context) (*lease.Stale, error) {
	var stale *lease.Stale
	err := r.h.Store.View(ctx, func(tx *eventstore.Tx) error { return fenceAttempt(tx, r.spec.Attempt, r.token) })
	if errors.As(err, &stale) {
		return stale, nil
	}
	if err != nil {
		return nil, hostFault(ErrRecord, err)
	}
	return nil, nil
}

func refusalHandle(h types.AdapterHandle) types.AttemptOutputRefusalHandle {
	return types.AttemptOutputRefusalHandle{Attempt: h.Attempt, Handle: h.Handle, Digest: h.Digest, Size: h.Size}
}

// refuse keeps a refusal for the final transaction. A declaration the
// refusal record cannot hold (a structurally decoded handle with a bad
// digest, say) is not recorded: the run is refused all the same (denied).
func (r *attemptRun) refuse(h types.AttemptOutputRefusalHandle, check types.AttemptOutputRefusalCheck, detail string) {
	r.denied = true
	_, rec, err := lease.Refusal(r.spec.Attempt, h, check, detail)
	if err != nil {
		return
	}
	if raw, err := json.Marshal(rec); err != nil || contract.Validate(lease.OutputRefusalSchema, raw) != nil {
		return
	}
	r.out.Refusals = append(r.out.Refusals, rec)
}

// finish records the attempt's end in one writer transaction (09 step 8):
// its usage, never fenced; then, when submit holds and nothing was
// refused, the fence again and attempt.result_submitted carrying the
// result's blob (running → finishing). Otherwise it
// records the refusals (a stale fence among them) and, for an attempt still
// running, attempt.lost: an intermediate state that closes publication (05),
// the refusals being the authoritative explanation. A failed submission is
// retried as the end without it.
func (r *attemptRun) finish(ctx context.Context, result *types.AdapterRunResult, cause error, submit bool) (Outcome, error) {
	r.out.Result, r.out.Session = result, cause
	usage, merr := r.measurements(result)
	if merr != nil {
		usage, submit, merr = nil, false, hostFault(ErrRecord, merr)
	}
	pending := r.out.Refusals
	// Fail closed: only a result whose every output was checked, copied
	// and not refused is submitted, and only with its result's copy.
	submitting := submit && result != nil && r.admitted && !r.denied && len(pending) == 0
	if submitting && result.Result != nil && r.blobs[result.Result.Handle] == "" {
		submitting, r.denied = false, true
	}
	var submitted *types.SubmittedResult
	_, err := r.h.command(ctx, r.spec.Attempt, "finish", func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		submitted, r.out.Refusals = nil, append([]types.AttemptOutputRefusal(nil), pending...)
		var evs []eventstore.Event
		for _, m := range usage {
			ev, err := ledger.Measure(m)
			if err != nil {
				return nil, err
			}
			evs = append(evs, ev)
		}
		if submitting {
			sub := r.submitted(*result)
			sevs, err := lease.SubmitResult(tx, r.spec.Attempt, r.token, sub)
			var stale *lease.Stale
			switch {
			case errors.As(err, &stale):
				for h := range r.blobs {
					r.refuse(refusalHandle(h), types.AttemptOutputRefusalCheckStaleAttempt, stale.Detail)
				}
			case err != nil:
				return nil, err
			default:
				submitted = &sub
				return append(evs, sevs...), nil
			}
		}
		for _, f := range r.out.Refusals {
			ev, _, err := lease.Refusal(f.Attempt, f.Handle, f.Check, f.Detail)
			if err != nil {
				return nil, err
			}
			evs = append(evs, ev)
		}
		lost, err := lease.LoseRunning(tx, r.spec.Attempt, lostReason(cause))
		return append(evs, lost...), err
	})
	if err != nil {
		r.out.Refusals = pending
		if submitting {
			out, ferr := r.finish(ctx, result, ErrRecord, false)
			return out, errors.Join(hostFault(ErrRecord, err), ferr)
		}
		return r.out, hostFault(ErrRecord, err)
	}
	r.out.Submitted, r.out.Usage = submitted, usage
	return r.out, merr
}

// submitted is the result as attempt.result_submitted records it.
func (r *attemptRun) submitted(res types.AdapterRunResult) types.SubmittedResult {
	s := types.SubmittedResult{Call: r.spec.Call, Outcome: res.Outcome, Reason: res.Reason}
	if res.Result == nil {
		return s
	}
	k := types.SubmittedResultKindValue(res.Result.Kind)
	if d, ok := r.blobs[res.Result.Handle]; ok {
		s.Kind, s.Blob = &k, &d
	}
	return s
}

// lostReason: silence is a dead stream, anything else a disconnect.
func lostReason(cause error) types.AttemptLostReasonValue {
	var e *session.Error
	if errors.As(cause, &e) && e.Kind == session.KindHeartbeat {
		return types.AttemptLostReasonValueStreamDead
	}
	return types.AttemptLostReasonValueAgentDisconnect
}

// measurements builds the UsageMeasurements of the run's usage events
// (delta) and final usage (final; also a refused result's, when it was
// schema-valid), source adapter_report: the controller mints the ids and
// takes the lease and attempt from the fenced envelope. The ids are minted
// once, so a retried finalisation replays them.
func (r *attemptRun) measurements(result *types.AdapterRunResult) ([]types.UsageMeasurement, error) {
	if r.measured != nil {
		return r.measured, nil
	}
	r.mu.Lock()
	events := append([]*types.UsageEvent(nil), r.usage...)
	r.mu.Unlock()
	out := []types.UsageMeasurement{}
	add := func(kind types.UsageMeasurementKind, seq int64, tokens types.UsageMeasurementTokens, turns, ms types.Amount, cost types.UsageMeasurementCost) error {
		id, err := ids.New("msr")
		out = append(out, types.UsageMeasurement{ID: types.MeasurementID(id), Call: r.spec.Call, Attempt: r.spec.Attempt, Lease: r.spec.Lease,
			Kind: kind, Sequence: seq, Tokens: tokens, Turns: turns, Milliseconds: ms, Cost: cost,
			SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport})
		return err
	}
	for _, u := range events {
		if err := add(types.UsageMeasurementKindDelta, u.Sequence, u.Tokens, u.Turns, u.Milliseconds, u.Cost); err != nil {
			return nil, err
		}
	}
	f := r.declaredFinal
	if result != nil {
		f = result.UsageFinal
	}
	if f != nil {
		if err := add(types.UsageMeasurementKindFinal, 0, f.Tokens, f.Turns, f.Milliseconds, f.Cost); err != nil {
			return nil, err
		}
	}
	r.measured = out
	return out, nil
}
