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
// expected state and scope, and who proposed it.
type Bound struct {
	Action        string
	Classes       []string
	Project       *types.ProjectID
	Object        types.ObjectRef
	ExpectedState types.ExpectedState
	Scope         json.RawMessage
	ProposedBy    types.ActorID
}

// BindingDigest is the JCS digest of {action, change_class, project,
// object, expected_state, scope, proposed_by} (04). Classes are sorted, so
// their order never changes the digest.
func BindingDigest(b Bound) (types.Digest, error) {
	classes := slices.Clone(b.Classes)
	slices.Sort(classes)
	classes = slices.Compact(classes)
	if classes == nil {
		classes = []string{}
	}
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
	}{b.Action, classes, b.Project, b.Object, b.ExpectedState, b.Scope, b.ProposedBy})
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

// NewProposal records a Propose decision as an open proposal, bound to its
// digest, expiring at expires.
func NewProposal(id types.ProposalID, d Decision, b Bound, expires time.Time) (types.Proposal, error) {
	if d.Disposition != Propose || d.Policy == None {
		return types.Proposal{}, errors.New("authority: only a decision to propose records a proposal")
	}
	b.Classes = d.Classes
	digest, err := BindingDigest(b)
	if err != nil {
		return types.Proposal{}, err
	}
	classes := make([]types.ChangeClass, 0, len(b.Classes))
	for _, c := range b.Classes {
		classes = append(classes, types.ChangeClass(c))
	}
	return types.Proposal{ID: id, Action: types.ActionKind(b.Action), ChangeClass: classes, Project: b.Project, Object: b.Object,
		ExpectedState: b.ExpectedState, Scope: b.Scope, ProposedBy: b.ProposedBy, Grant: d.Grant, Policy: types.ProposalPolicy(d.Policy.String()),
		BindingDigest: digest, Status: types.ProposalStatusOpen, ExpiresAt: types.Time(expires.UTC().Format(time.RFC3339))}, nil
}

// open refuses a proposal that is not open, or open but past its expiry.
func open(p types.Proposal, now time.Time) error {
	if p.Status != types.ProposalStatusOpen {
		return refusal(vocab.ReasonProposalNotOpen, "proposal %s is %s", p.ID, p.Status)
	}
	until, err := time.Parse(time.RFC3339Nano, string(p.ExpiresAt))
	if err != nil || !now.Before(until) {
		return refusal(vocab.ReasonProposalExpired, "proposal %s expired at %s", p.ID, p.ExpiresAt)
	}
	return nil
}

// Close moves an open proposal to a final status; only an open one moves.
func Close(p types.Proposal, to types.ProposalStatus) (types.Proposal, error) {
	if p.Status != types.ProposalStatusOpen || to == types.ProposalStatusOpen {
		return types.Proposal{}, refusal(vocab.ReasonProposalNotOpen, "proposal %s is %s, not open", p.ID, p.Status)
	}
	n := p
	n.Status = to
	return n, nil
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

// ConsumeChallenge uses a challenge once, for the proposal it was issued
// for, by one of its confirmers. The WebAuthn assertion over nonce ‖
// binding_digest is verified by the dashboard before this (D37); this is
// the single-use, binding and confirmer check that commits with it.
func ConsumeChallenge(c types.Challenge, p types.Proposal, confirmer types.Actor, now time.Time) (types.Challenge, error) {
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
func ConfirmByReview(p types.Proposal, r types.Receipt, c types.InstrumentContract, blobs receipt.Blobs, excluded []string, now time.Time) error {
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
// unrevoked operator. Any change invalidates the proposal, never applies it.
func Revalidate(reg *vocab.Registry, p types.Proposal, confirmer *types.Actor, current Request, state types.ExpectedState) error {
	grant := p.Grant
	if p.Status != types.ProposalStatusConfirmed {
		return refusal(vocab.ReasonProposalNotOpen, "proposal %s is %s, not confirmed", p.ID, p.Status)
	}
	if !sameState(p.ExpectedState, state) {
		return refusal(vocab.ReasonStaleExpectedState, "proposal %s was decided against another state", p.ID)
	}
	if current.Actor.ID != p.ProposedBy || current.Action != string(p.Action) {
		return refusal(vocab.ReasonProposalInvalidated, "the request is not proposal %s's", p.ID)
	}
	if (current.Project == nil) != (p.Project == nil) || (current.Project != nil && *current.Project != *p.Project) {
		return refusal(vocab.ReasonProposalInvalidated, "the request is for another project than proposal %s", p.ID)
	}
	d := Decide(reg, current)
	if d.Disposition != Refuse && !sameClasses(d.Classes, p.ChangeClass) {
		return refusal(vocab.ReasonProposalInvalidated, "proposal %s: the change's classes are now %v, not %v", p.ID, d.Classes, p.ChangeClass)
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
	return nil
}

// sameClasses compares the re-decided classes with the proposal's, as sets.
func sameClasses(got []string, want []types.ChangeClass) bool {
	g := slices.Compact(slices.Sorted(slices.Values(got)))
	w := make([]string, 0, len(want))
	for _, c := range want {
		w = append(w, string(c))
	}
	return slices.Equal(g, slices.Compact(slices.Sorted(slices.Values(w))))
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
