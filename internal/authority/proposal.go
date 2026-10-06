// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/gowebpki/jcs"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority/webauthn"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// ChallengeTTL is a human challenge's lifetime (04: short, re-issued on
// demand).
const ChallengeTTL = 15 * time.Minute

// Refusal is a proposal or confirmation step authority refuses.
type Refusal struct {
	Reason vocab.Reason
	Detail string
}

func (r *Refusal) Error() string { return "authority: " + r.Reason.String() + ": " + r.Detail }

func refusal(reason vocab.Reason, format string, args ...any) *Refusal {
	return &Refusal{reason, fmt.Sprintf(format, args...)}
}

// Bound is what a proposal binds: the exact action, its change, object,
// expected state and scope (the command's input), who proposed it and, for
// a grant change, its grantee.
type Bound struct {
	Action        string
	Classes       []string
	Project       *types.ProjectID
	Object        types.ObjectRef
	ExpectedState types.ExpectedState
	Scope         json.RawMessage
	ProposedBy    types.ActorID
	Grantee       *types.ActorID
}

// BindingDigest is the JCS digest of {action, change_class, project,
// object, expected_state, scope, proposed_by} (04), and the grantee when
// there is one (absent otherwise, so a digest without one is unchanged).
// Classes are sorted, so their order never changes the digest.
func BindingDigest(b Bound) (types.Digest, error) {
	classes := canonical(b.Classes)
	if len(b.Scope) == 0 {
		return "", errors.New("authority: a proposal binds its scope")
	}
	raw, err := json.Marshal(struct {
		Action        string              `json:"action"`
		ChangeClass   []string            `json:"change_class"`
		Project       *types.ProjectID    `json:"project"`
		Object        types.ObjectRef     `json:"object"`
		ExpectedState types.ExpectedState `json:"expected_state"`
		Scope         json.RawMessage     `json:"scope"`
		ProposedBy    types.ActorID       `json:"proposed_by"`
		Grantee       *types.ActorID      `json:"grantee,omitempty"`
	}{b.Action, classes, b.Project, b.Object, b.ExpectedState, b.Scope, b.ProposedBy, b.Grantee})
	if err != nil {
		return "", err
	}
	c, err := jcs.Transform(raw)
	if err != nil {
		return "", fmt.Errorf("authority: binding: %w", err)
	}
	sum := sha256.Sum256(c)
	return types.Digest("sha256:" + hex.EncodeToString(sum[:])), nil
}

// canonical is a class list sorted and without repeats: the form both the
// digest and the proposal record use.
func canonical(classes []string) []string {
	c := slices.Compact(slices.Sorted(slices.Values(classes)))
	if c == nil {
		c = []string{}
	}
	return c
}

// NewProposal records a Propose decision as an open proposal, bound to its
// digest, created at created and expiring at expires.
func NewProposal(id types.ProposalID, d Decision, b Bound, created, expires time.Time) (types.Proposal, error) {
	if d.Disposition != Propose || d.Policy == None {
		return types.Proposal{}, errors.New("authority: only a decision to propose records a proposal")
	}
	b.Classes = canonical(d.Classes)
	digest, err := BindingDigest(b)
	if err != nil {
		return types.Proposal{}, err
	}
	classes := make([]types.ChangeClass, 0, len(b.Classes))
	for _, c := range b.Classes {
		classes = append(classes, types.ChangeClass(c))
	}
	return types.Proposal{ID: id, Action: types.ActionKind(b.Action), ChangeClass: classes, Project: b.Project, Object: b.Object,
		ExpectedState: b.ExpectedState, Scope: b.Scope, ProposedBy: b.ProposedBy, Grantee: b.Grantee, Grant: d.Grant,
		Policy: types.ProposalPolicy(d.Policy.String()), BindingDigest: digest, Status: types.ProposalStatusOpen,
		CreatedAt: types.Time(created.UTC().Format(time.RFC3339Nano)), ExpiresAt: types.Time(expires.UTC().Format(time.RFC3339Nano))}, nil
}

// open refuses a proposal that is not open, or open but past its expiry.
func open(p types.Proposal, now time.Time) error {
	if p.Status != types.ProposalStatusOpen && !mutation.Is(mutation.ConfirmIgnoresStatus) {
		return refusal(vocab.ReasonProposalNotOpen, "proposal %s is %s", p.ID, p.Status)
	}
	until, err := time.Parse(time.RFC3339Nano, string(p.ExpiresAt))
	if err != nil || !now.Before(until) {
		return refusal(vocab.ReasonProposalExpired, "proposal %s expired at %s", p.ID, p.ExpiresAt)
	}
	return nil
}

// Close ends an open proposal as rejected, expired or superseded. A
// proposal is confirmed only through ConfirmHuman or ConfirmByReview, and
// invalidated only through Invalidate.
func Close(p types.Proposal, to types.ProposalStatus) (types.Proposal, error) {
	if p.Status != types.ProposalStatusOpen {
		return types.Proposal{}, refusal(vocab.ReasonProposalNotOpen, "proposal %s is %s, not open", p.ID, p.Status)
	}
	if !slices.Contains([]types.ProposalStatus{types.ProposalStatusRejected, types.ProposalStatusExpired, types.ProposalStatusSuperseded}, to) {
		return types.Proposal{}, fmt.Errorf("authority: a proposal is not closed as %s", to)
	}
	n := p
	n.Status = to
	return n, nil
}

// Invalidate ends an open or confirmed proposal for good, for a registered
// reason: the writer persists it whenever revalidation fails, so a later
// change (a restored grant, say) can never make it applicable again.
func Invalidate(p types.Proposal, reason types.ProposalInvalidatedReasonValue) (types.Proposal, error) {
	if p.Status != types.ProposalStatusOpen && p.Status != types.ProposalStatusConfirmed {
		return types.Proposal{}, refusal(vocab.ReasonProposalNotOpen, "proposal %s is %s", p.ID, p.Status)
	}
	n := p
	n.Status, n.InvalidatedReason = types.ProposalStatusInvalidated, &reason
	return n, nil
}

// confirmed returns the proposal confirmed.
func confirmed(p types.Proposal) types.Proposal {
	n := p
	n.Status = types.ProposalStatusConfirmed
	return n
}

// IssueChallenge issues a single-use human challenge for an open
// human-policy proposal: 32 fresh random bytes, bound to its digest, for
// the given operators (none revoked), expiring after ChallengeTTL.
func IssueChallenge(id types.ChallengeID, p types.Proposal, confirmers []types.Actor, random io.Reader, now time.Time) (types.Challenge, error) {
	if err := open(p, now); err != nil {
		return types.Challenge{}, err
	}
	if p.Policy != types.ProposalPolicyHuman {
		return types.Challenge{}, refusal(vocab.ReasonConfirmationRefused, "proposal %s is confirmed by %s, not a human", p.ID, p.Policy)
	}
	var allowed []types.ActorID
	for _, a := range confirmers {
		if a.Kind != types.ActorKindOperator || revoked(a, now) {
			return types.Challenge{}, refusal(vocab.ReasonConfirmationRefused, "actor %s may not confirm", a.ID)
		}
		allowed = append(allowed, a.ID)
	}
	if len(allowed) == 0 {
		return types.Challenge{}, refusal(vocab.ReasonConfirmationRefused, "a challenge names its confirmers")
	}
	var nonce [32]byte
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		return types.Challenge{}, fmt.Errorf("authority: challenge nonce: %w", err)
	}
	return types.Challenge{ID: id, Proposal: p.ID, BindingDigest: p.BindingDigest, Nonce: hex.EncodeToString(nonce[:]),
		AllowedConfirmers: allowed, ExpiresAt: types.Time(now.Add(ChallengeTTL).UTC().Format(time.RFC3339))}, nil
}

// ConfirmHuman confirms an open, unexpired human-policy proposal with a
// WebAuthn assertion for its challenge (04, D37): the assertion is verified
// against the confirmer's registered credential and rp over the challenge's
// nonce ‖ binding_digest, then the challenge is consumed, the proposal
// confirmed and the credential's signature counter advanced together, for
// the writer to persist in one transaction.
func ConfirmHuman(p types.Proposal, c types.Challenge, confirmer types.Actor, cred webauthn.Credential, rp webauthn.RelyingParty,
	a webauthn.Assertion, now time.Time) (types.Proposal, types.Challenge, webauthn.Credential, error) {
	none := func(err error) (types.Proposal, types.Challenge, webauthn.Credential, error) {
		return types.Proposal{}, types.Challenge{}, webauthn.Credential{}, err
	}
	if p.Policy != types.ProposalPolicyHuman {
		return none(refusal(vocab.ReasonConfirmationRefused, "proposal %s is confirmed by %s, not a human", p.ID, p.Policy))
	}
	if cred.Actor != confirmer.ID {
		return none(refusal(vocab.ReasonConfirmationRefused, "the credential is not %s's", confirmer.ID))
	}
	challenge, err := webauthn.Challenge(c.Nonce, c.BindingDigest)
	if err != nil {
		return none(refusal(vocab.ReasonConfirmationRefused, "challenge %s: %v", c.ID, err))
	}
	count, err := webauthn.Verify(rp, cred, challenge, a)
	if err != nil {
		return none(refusal(vocab.ReasonConfirmationRefused, "challenge %s: %v", c.ID, err))
	}
	used, err := consumeChallenge(c, p, confirmer, now)
	if err != nil {
		return none(err)
	}
	n := cred
	n.SignCount = count
	return confirmed(p), used, n, nil
}

// consumeChallenge uses a challenge once, for the proposal it was issued
// for, by one of its confirmers.
func consumeChallenge(c types.Challenge, p types.Proposal, confirmer types.Actor, now time.Time) (types.Challenge, error) {
	if err := open(p, now); err != nil {
		return types.Challenge{}, err
	}
	until, err := time.Parse(time.RFC3339Nano, string(c.ExpiresAt))
	switch {
	case c.UsedAt != nil:
		return types.Challenge{}, refusal(vocab.ReasonConfirmationRefused, "challenge %s was used at %s", c.ID, *c.UsedAt)
	case err != nil || !now.Before(until):
		return types.Challenge{}, refusal(vocab.ReasonConfirmationRefused, "challenge %s expired", c.ID)
	case c.Proposal != p.ID || c.BindingDigest != p.BindingDigest:
		return types.Challenge{}, refusal(vocab.ReasonConfirmationRefused, "challenge %s is not for this proposal's binding", c.ID)
	case confirmer.Kind != types.ActorKindOperator || revoked(confirmer, now) || !slices.Contains(c.AllowedConfirmers, confirmer.ID):
		return types.Challenge{}, refusal(vocab.ReasonConfirmationRefused, "actor %s may not confirm challenge %s", confirmer.ID, c.ID)
	}
	n := c
	used := types.Time(now.UTC().Format(time.RFC3339Nano))
	n.UsedAt = &used
	return n, nil
}

// ConfirmByReview decides whether a review receipt confirms an open
// independent_review proposal (04): checked under its instrument, about
// exactly this proposal's binding digest, independent of the proposer's
// and every contributor's model families, and a derived GO.
//
// excluded must hold the proposer's and every contributor's families, from
// authoritative provenance; an unknown one is passed as "unknown" (and
// blocks), never left out.
func ConfirmByReview(p types.Proposal, r types.Receipt, c types.InstrumentContract, blobs receipt.Blobs, excluded []string, now time.Time) (types.Proposal, error) {
	if err := confirmReview(p, r, c, blobs, excluded, now); err != nil {
		return types.Proposal{}, err
	}
	return confirmed(p), nil
}

func confirmReview(p types.Proposal, r types.Receipt, c types.InstrumentContract, blobs receipt.Blobs, excluded []string, now time.Time) error {
	if err := open(p, now); err != nil {
		return err
	}
	if p.Policy != types.ProposalPolicyIndependentReview {
		return refusal(vocab.ReasonConfirmationRefused, "proposal %s is confirmed by %s, not a review", p.ID, p.Policy)
	}
	if err := receipt.Check(c, r, blobs); err != nil {
		return err
	}
	subject := types.Subject{Variant: &types.ProposalSubject{Type: "proposal", Proposal: p.ID, BindingDigest: p.BindingDigest}}
	req := receipt.Requirement{Kind: types.ReceiptKindReview, Instrument: r.Instrument, Subject: subject}
	if err := receipt.Applies(r, req, now); err != nil {
		return err
	}
	if err := receipt.Independent(r, excluded); err != nil {
		return err
	}
	if r.Verdict == nil || *r.Verdict != types.ReceiptVerdictValueGo {
		return refusal(vocab.ReasonConfirmationRefused, "review %s is not a GO", r.ID)
	}
	return nil
}

// Revalidate re-decides a confirmed proposal at apply, in the writer
// transaction (04): the object must still be in its expected state; the
// same action, project and change must still be proposable under the same
// policy and under the very grant the proposal records (never a substitute,
// never one the caller names); and a human confirmer must still be an
// unrevoked operator, and any confirmer still separate from the proposal
// (Separate). Any change invalidates the proposal, never applies it.
//
// op is the operation being applied (its object and scope); with the
// request's actor, action, project and re-decided classes it must hash to
// the proposal's binding digest, and so must the proposal record itself.
// Any mismatch returns proposal_invalidated (a moved object returns
// stale_expected_state); the writer then persists Invalidate.
func Revalidate(reg *vocab.Registry, p types.Proposal, confirmer *types.Actor, current Request, op Bound, state types.ExpectedState) error {
	grant := p.Grant
	if p.Status != types.ProposalStatusConfirmed {
		return refusal(vocab.ReasonProposalNotOpen, "proposal %s is %s, not confirmed", p.ID, p.Status)
	}
	if !sameState(p.ExpectedState, state) {
		return refusal(vocab.ReasonStaleExpectedState, "proposal %s was decided against another state", p.ID)
	}
	if got, err := BindingDigest(boundOf(p)); err != nil || got != p.BindingDigest {
		return refusal(vocab.ReasonProposalInvalidated, "proposal %s does not match its own binding digest", p.ID)
	}
	d := Decide(reg, current)
	if d.Disposition != Refuse {
		applied := Bound{Action: current.Action, Classes: d.Classes, Project: current.Project, Object: op.Object,
			ExpectedState: p.ExpectedState, Scope: op.Scope, ProposedBy: current.Actor.ID, Grantee: current.Grantee}
		if got, err := BindingDigest(applied); err != nil || got != p.BindingDigest {
			return refusal(vocab.ReasonProposalInvalidated, "proposal %s: the operation applied is not the one confirmed", p.ID)
		}
	}
	switch {
	case d.Disposition == Refuse:
		return refusal(vocab.ReasonProposalInvalidated, "proposal %s: %s: %s", p.ID, d.Reason.String(), d.Detail)
	case d.Disposition == Propose && d.Policy.String() != string(p.Policy):
		return refusal(vocab.ReasonProposalInvalidated, "proposal %s: the policy is now %s, not %s", p.ID, d.Policy, p.Policy)
	case d.Disposition == Allow:
		return refusal(vocab.ReasonProposalInvalidated, "proposal %s: the action no longer needs this confirmation", p.ID)
	case (grant == nil) != (d.Grant == nil) || (grant != nil && *grant != *d.Grant):
		return refusal(vocab.ReasonProposalInvalidated, "proposal %s: the grant it was proposed under no longer covers it", p.ID)
	}
	if p.Policy == types.ProposalPolicyHuman && (confirmer == nil || confirmer.Kind != types.ActorKindOperator || revoked(*confirmer, current.Now)) {
		return refusal(vocab.ReasonProposalInvalidated, "proposal %s: its confirmer is no longer an operator in force", p.ID)
	}
	// Separation of duties again at apply, so no later change launders a
	// self-approval; the caller passes the approval's actor whenever there is one.
	if confirmer != nil {
		if err := Separate(p, *confirmer); err != nil {
			return refusal(vocab.ReasonProposalInvalidated, "proposal %s: %v", p.ID, err)
		}
	}
	return nil
}

// boundOf is what the proposal record binds.
func boundOf(p types.Proposal) Bound {
	b := Bound{Action: string(p.Action), Project: p.Project, Object: p.Object, ExpectedState: p.ExpectedState, Scope: p.Scope, ProposedBy: p.ProposedBy, Grantee: p.Grantee}
	for _, c := range p.ChangeClass {
		b.Classes = append(b.Classes, string(c))
	}
	return b
}

func sameState(a, b types.ExpectedState) bool {
	eqInt := func(x, y *int64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	eqDig := func(x, y *types.Digest) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return eqInt(a.Version, b.Version) && eqDig(a.Digest, b.Digest)
}

func revoked(a types.Actor, now time.Time) bool {
	if a.RevokedAt == nil {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, string(*a.RevokedAt))
	return err != nil || !now.Before(at)
}
