// SPDX-License-Identifier: Apache-2.0

package graph

// Content classes and isolation (12 "Content"; plan 15 E3).
//
// Decisions:
//   - An evidence result is published (recorded as anything but
//     could_not_run, and so selectable) only with the isolation proof of its
//     attempt's run (evidence.CheckIsolation). CommitEvidence records a
//     result the proof does not prove as could_not_run/isolation_unproven,
//     with no value; a proof that is not a records/isolation-proof is
//     dropped from that record. The result.recorded fold refuses, from the
//     log alone, a published evidence result without such a proof, a proof
//     on another kind's result, a second result of one attempt, and an
//     attempt not started on the node's unit (M1 P8a, units.go).
//   - The committer's proof is trusted: nothing binds the probe's output
//     to a digest until the evidence surface activates (#187, #191).
//   - isolation_unproven spends an attempt: the probe ran inside the
//     wrapper, so something ran (12 "What counts", the conservative side).
//   - An artifact's class is the producing node's instrument contract's
//     (06 rule 7), and the result's receipt must be that instrument's:
//     recordResult and Context refuse another. In an evidence result, every
//     ref but the receipt itself, by identity, is an artifact: it reaches a
//     controller context only when the receipt names its blob as an
//     artifact the contract classes diagnostic. A restricted artifact, one
//     the contract does not class, a blob named under two classes and a ref
//     the receipt does not name are omitted as restricted. Gate results are
//     not classed in M0: a gate runs in the sandbox, and Env.Contracts holds
//     evidence instruments only.
//   - Context applies this itself, so no caller decides it: what a manifest
//     includes is Rendered.Included, and the restricted refs are named in
//     Rendered.Omitted, never cut silently (10).

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/evidence"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// AttemptResultProjection is attempt id → the one result it recorded.
const AttemptResultProjection = "graph_attempt_results"

// maxDetail bounds an isolation refusal's detail, under records/result's.
const maxDetail = 1024

// Rendered is a node's controller context: the refs a manifest.intent
// includes, in declared order, and the refs it omits as restricted.
type Rendered struct {
	Included []types.Ref
	Omitted  []types.ManifestIntentOmittedItem
}

// CommitEvidence is Commit for r, the result of attempt of an evidence
// node, whose isolation probe observed proof. A result that is not
// could_not_run and that proof does not prove isolated is recorded as
// could_not_run/isolation_unproven instead. It returns the result recorded.
func CommitEvidence(tx *eventstore.Tx, node types.NodeID, r types.Result, attempt types.AttemptID, proof types.IsolationProof, env Env) (types.Result, error) {
	isolation := &proof
	b, err := json.Marshal(proof)
	if err == nil {
		err = contract.Validate("records/isolation-proof", b)
	}
	if err != nil {
		isolation, err = nil, fmt.Errorf("the isolation proof is not a records/isolation-proof: %w", err)
	} else {
		err = evidence.CheckIsolation(evidence.RunOf(attempt), proof)
	}
	if err != nil && r.Outcome != types.OutcomeCouldNotRun {
		detail := err.Error()
		if len(detail) > maxDetail {
			detail = detail[:maxDetail]
		}
		r = types.Result{ID: r.ID, Computation: r.Computation, Kind: r.Kind, Value: []types.Ref{}, Outcome: types.OutcomeCouldNotRun,
			Reason: vocab.ReasonIsolationUnproven, Detail: detail}
	}
	return r, commitRecorded(tx, types.ResultRecorded{Node: node, Result: r, Attempt: &attempt, Isolation: isolation}, env)
}

// CommitAttempt is Commit for r, the result attempt recorded: the one way
// a node an attempt still runs records its result (Astra #316 H1). The
// fold binds the result to the attempt (AttemptResultProjection), which
// must be started on node's unit, have submitted its result, and have
// recorded no other.
func CommitAttempt(tx *eventstore.Tx, node types.NodeID, r types.Result, attempt types.AttemptID, env Env) error {
	return commitRecorded(tx, types.ResultRecorded{Node: node, Result: r, Attempt: &attempt}, env)
}

// AttemptResult is the result attempt itself recorded, if any: never one
// another attempt, or no attempt, recorded on its node. An index naming no
// result, or another attempt's, is ErrIndexDamaged (Opus #432 r2 H1′).
func AttemptResult(tx *eventstore.Tx, attempt types.AttemptID) (types.Result, bool, error) {
	id, ok, err := load[types.ResultID](tx, AttemptResultProjection, string(attempt))
	if err != nil || !ok {
		return types.Result{}, false, err
	}
	r, ok, err := load[types.ResultRecorded](tx, ResultProjection, string(id))
	if err == nil && (!ok || r.Attempt == nil || *r.Attempt != attempt || r.Result.ID != id) {
		err = fmt.Errorf("%w: attempt %s's result index names %s, not a result it recorded", ErrIndexDamaged, attempt, id)
	}
	return r.Result, err == nil, err
}

// isolated is the result.recorded fold's check of p's attempt and
// isolation proof.
func isolated(tx *eventstore.Tx, rec nodeRecord, p *types.ResultRecorded) error {
	// A node an attempt still runs records that attempt's result alone
	// (Astra #316 H1). Evidence keeps its own rule below (a published result
	// needs its attempt and proof); adcd records no evidence node (I15).
	if p.Attempt == nil && rec.Recorded.Node.Kind != types.NodeKindEvidence {
		if cur, started, err := lease.CurrentAttempt(tx, UnitOf(p.Node)); err != nil {
			return err
		} else if started {
			a, _, _, err := lease.AttemptOf(tx, cur)
			if err != nil {
				return err
			}
			switch a.Status {
			case types.AttemptStatusRunning, types.AttemptStatusStopping, types.AttemptStatusFinishing:
				return fmt.Errorf("attempt %s is %s on node %s: a result with no attempt is not recorded while it runs", cur, a.Status, p.Node)
			}
		}
	}
	if p.Attempt != nil {
		if _, used, err := load[types.ResultID](tx, AttemptResultProjection, string(*p.Attempt)); err != nil || used {
			return errors.Join(err, fmt.Errorf("attempt %s already recorded a result", *p.Attempt))
		}
		if err := attemptBound(tx, p.Node, *p.Attempt); err != nil {
			return err
		}
		if err := put(tx, AttemptResultProjection, string(*p.Attempt), p.Result.ID); err != nil {
			return err
		}
	}
	switch {
	case rec.Recorded.Node.Kind != types.NodeKindEvidence:
		if p.Isolation != nil {
			return errors.New("only an evidence result carries an isolation proof")
		}
		return nil
	case p.Result.Outcome == types.OutcomeCouldNotRun:
		return nil
	case p.Attempt == nil || p.Isolation == nil:
		return errors.New("an evidence result is published only with its run's isolation proof")
	}
	return evidence.CheckIsolation(evidence.RunOf(*p.Attempt), *p.Isolation)
}

// sameInstrument: rcpt, held by a result of rec, is rec's instrument's
// when rec is an evidence node.
func sameInstrument(rec nodeRecord, rcpt types.Receipt) error {
	if rec.Recorded.Node.Kind != types.NodeKindEvidence {
		return nil
	}
	p, err := rec.Recorded.Node.ParamsAsEvidenceNodeParams()
	if err == nil && rcpt.Instrument != p.Instrument {
		err = fmt.Errorf("graph: the receipt is %s's, not the evidence node's instrument %s", rcpt.Instrument.Name, p.Instrument.Name)
	}
	return err
}

// classed splits rr's refs into those a controller context renders and
// those it omits as restricted.
func classed(tx *eventstore.Tx, rr types.ResultRecorded, env Env, out *Rendered) error {
	if rr.Result.Kind != types.ResultKindEvidence || len(rr.Result.Value) == 0 || mutation.Is(mutation.RestrictedArtifactRendered) {
		out.Included = append(out.Included, rr.Result.Value...)
		return nil
	}
	ref, classes, err := artifactClasses(tx, rr, env)
	if err != nil {
		return err
	}
	for _, r := range rr.Result.Value {
		switch {
		case r.IdentityDigest == ref.IdentityDigest, classes[r.ContentDigest] == types.InstrumentContractArtifactClassesValueDiagnostic:
			out.Included = append(out.Included, r)
		default:
			out.Omitted = append(out.Omitted, types.ManifestIntentOmittedItem{Ref: r, Reason: types.ManifestIntentOmittedItemReasonRestricted})
		}
	}
	return nil
}

// artifactClasses are rr's receipt Ref and the classes of the artifacts
// its receipt names, by blob, as the producing node's contract classes
// them: restricted when the contract does not class one, or classes a
// blob named twice differently.
func artifactClasses(tx *eventstore.Tx, rr types.ResultRecorded, env Env) (types.Ref, map[types.Digest]types.InstrumentContractArtifactClassesValue, error) {
	ref, rcpt, err := receiptOf(rr, env.Blobs)
	rec, nerr := loadNode(tx, rr.Node)
	if err = errors.Join(err, nerr); err != nil {
		return ref, nil, err
	}
	c, err := contractOf(rec, env.Contracts)
	if err = errors.Join(err, sameInstrument(rec, rcpt)); err != nil {
		return ref, nil, err
	}
	out := map[types.Digest]types.InstrumentContractArtifactClassesValue{}
	for _, p := range rcpt.Predicates {
		for _, ev := range p.Evidence {
			class, ok := c.ArtifactClasses[string(ev.Artifact)]
			if prev, seen := out[ev.Blob]; !ok || seen && prev != class {
				class = types.InstrumentContractArtifactClassesValueRestricted
			}
			out[ev.Blob] = class
		}
	}
	return ref, out, nil
}
