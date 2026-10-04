// SPDX-License-Identifier: Apache-2.0

package receipt

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Refusal is why evidence does not serve a decision: a could_not_run with
// a registered reason (rule 8: missing evidence never passes).
type Refusal struct {
	Reason vocab.Reason
	Detail string
}

func (r *Refusal) Error() string { return "receipt: " + r.Reason.String() + ": " + r.Detail }

// Requirement is what a decision needs from a receipt: the instrument that
// must have produced it (by digest), the typed subject it must be about,
// and the demand it must answer, if the requirement names one.
type Requirement struct {
	Instrument types.InstrumentRef
	Subject    types.Subject
	DemandKey  *types.Digest
}

// Applies decides whether a valid receipt applies to a requirement at time
// now (rules 1, 2, 6): the same instrument digest, the same typed subject,
// the same demand, and not past its validity. Anything else is a Refusal.
func Applies(r types.Receipt, req Requirement, now time.Time) error {
	if r.Instrument.Name != req.Instrument.Name {
		return &Refusal{vocab.ReasonEvidenceInapplicable, fmt.Sprintf("produced by %s, the requirement names %s", r.Instrument.Name, req.Instrument.Name)}
	}
	if r.Instrument.Digest != req.Instrument.Digest {
		return &Refusal{vocab.ReasonInstrumentChanged, fmt.Sprintf("%s changed from %s to %s; re-run it", r.Instrument.Name, r.Instrument.Digest, req.Instrument.Digest)}
	}
	if err := sameSubject(r.Subject, req.Subject); err != nil {
		return &Refusal{vocab.ReasonEvidenceInapplicable, err.Error()}
	}
	if req.DemandKey != nil {
		d := demandOf(r.Subject)
		if d == nil || d.Key != *req.DemandKey {
			return &Refusal{vocab.ReasonEvidenceInapplicable, "the receipt answers another demand"}
		}
	}
	if r.ValidUntil != nil {
		until, err := time.Parse(time.RFC3339Nano, string(*r.ValidUntil))
		if err != nil {
			return fmt.Errorf("%w: valid_until: %v", ErrInvalid, err)
		}
		if !now.Before(until) {
			return &Refusal{vocab.ReasonEvidenceInapplicable, "the receipt expired at " + string(*r.ValidUntil)}
		}
	}
	return nil
}

// sameSubject compares typed subjects without their demand (checked
// separately): for a tree, the project, tree, environment identity and
// contract revision; for a proposal, the proposal and its binding digest
// (rule 4); for an object revision, the exact object; for an environment,
// its identity; for a call, the call.
func sameSubject(got, want types.Subject) error {
	switch w := want.Variant.(type) {
	case *types.TreeSubject:
		g, ok := got.Variant.(*types.TreeSubject)
		switch {
		case !ok:
			return errors.New("the receipt is not about a tree")
		case g.Project != w.Project, g.Tree != w.Tree:
			return fmt.Errorf("about tree %s of %s, the decision is about %s of %s", g.Tree, g.Project, w.Tree, w.Project)
		case g.Environment.Identity != w.Environment.Identity:
			return errors.New("about another environment")
		case g.ContractRevision != w.ContractRevision:
			return fmt.Errorf("under contract revision %s, the decision is under %s", g.ContractRevision, w.ContractRevision)
		}
	case *types.ProposalSubject:
		g, ok := got.Variant.(*types.ProposalSubject)
		if !ok || g.Proposal != w.Proposal || g.BindingDigest != w.BindingDigest {
			return errors.New("about another proposal or binding digest; an unrelated GO never applies")
		}
	case *types.ObjectRevisionSubject:
		g, ok := got.Variant.(*types.ObjectRevisionSubject)
		if !ok || g.Object != w.Object {
			return errors.New("about another object revision")
		}
	case *types.EnvironmentSubject:
		g, ok := got.Variant.(*types.EnvironmentSubject)
		if !ok || g.Identity != w.Identity {
			return errors.New("about another environment")
		}
	case *types.CallSubject:
		g, ok := got.Variant.(*types.CallSubject)
		if !ok || g.Call != w.Call {
			return errors.New("about another call")
		}
	default:
		return errors.New("the requirement names no subject")
	}
	return nil
}

// Independent decides whether a review receipt counts as independent of the
// given model families (the proposer's and every contributor's): its
// families must be known, recorded, and disjoint from them (rule 3). An
// unknown family, or none recorded, is not independent.
func Independent(r types.Receipt, excluded []string) error {
	fams := r.Provenance.ModelFamilies
	if len(fams) == 0 {
		return &Refusal{vocab.ReasonReviewerNotIndependent, "no model family is recorded"}
	}
	for _, f := range fams {
		switch {
		case f == "unknown":
			return &Refusal{vocab.ReasonReviewerNotIndependent, "a contributing model family is unknown"}
		case slices.Contains(excluded, f):
			return &Refusal{vocab.ReasonReviewerNotIndependent, "model family " + f + " also produced or contributed to the subject"}
		}
	}
	for _, f := range excluded {
		if f == "unknown" {
			return &Refusal{vocab.ReasonReviewerNotIndependent, "a proposer or contributor family is unknown"}
		}
	}
	return nil
}

// CanTeardown decides whether a run directory may be destroyed: every
// artifact the receipt names must already be in the blob store (rule 5).
func CanTeardown(r types.Receipt, blobs Blobs) error {
	for _, p := range r.Predicates {
		for _, e := range p.Evidence {
			ok, err := blobs.Has(e.Blob)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("receipt: artifact %s of %s (%s) is not in the blob store; teardown refused", e.Artifact, p.Name, e.Blob)
			}
		}
	}
	return nil
}
