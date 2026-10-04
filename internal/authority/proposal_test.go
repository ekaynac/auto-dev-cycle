// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	prpID = types.ProposalID("prp_01m3250vm7f6ktw39dszqnyddr")
	chlID = types.ChallengeID("chl_01m3250vz2ezn81cgvbkcn17jd")
	dgA   = types.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	dgB   = types.Digest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
)

func bound() Bound {
	return Bound{Action: "profile.adopt", Classes: []string{"policy.widen_destinations"}, Project: ptr(prjA),
		Object:        types.ObjectRef{Type: "profile", ID: string(prjA), Version: ptr(int64(4))},
		ExpectedState: types.ExpectedState{Version: ptr(int64(4))}, Scope: json.RawMessage(`{"revision":5}`), ProposedBy: supID}
}

func refusalOf(t *testing.T, err error) vocab.Reason {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return r.Reason
}

// The digest binds every field, not the order of classes.
func TestBindingDigest(t *testing.T) {
	base, err := BindingDigest(bound())
	if err != nil {
		t.Fatal(err)
	}
	two := bound()
	two.Classes = []string{"policy.widen_data", "policy.widen_destinations"}
	swapped := bound()
	swapped.Classes = []string{"policy.widen_destinations", "policy.widen_data"}
	a, _ := BindingDigest(two)
	b, _ := BindingDigest(swapped)
	if a != b || a == base {
		t.Fatalf("class order changed the digest, or the classes did not: %s %s %s", a, b, base)
	}
	for name, change := range map[string]func(*Bound){
		"action":         func(b *Bound) { b.Action = "task.create" },
		"project":        func(b *Bound) { b.Project = ptr(prjB) },
		"object":         func(b *Bound) { b.Object.Version = ptr(int64(5)) },
		"expected state": func(b *Bound) { b.ExpectedState = types.ExpectedState{Digest: ptr(dgA)} },
		"scope":          func(b *Bound) { b.Scope = json.RawMessage(`{"revision":6}`) },
		"proposer":       func(b *Bound) { b.ProposedBy = sup2ID },
	} {
		x := bound()
		change(&x)
		if d, err := BindingDigest(x); err != nil || d == base {
			t.Errorf("changing the %s kept the digest (%v)", name, err)
		}
	}
	noScope := bound()
	noScope.Scope = nil
	if _, err := BindingDigest(noScope); err == nil {
		t.Error("a proposal without its scope")
	}
}

func proposal(t *testing.T, policy Policy) types.Proposal {
	t.Helper()
	return proposalOf(t, policy, []string{"policy.widen_destinations"})
}

// proposalOf is an open proposal decided under grtA with the given classes.
func proposalOf(t *testing.T, policy Policy, classes []string) types.Proposal {
	t.Helper()
	b := bound()
	b.Classes = classes
	p, err := NewProposal(prpID, Decision{Disposition: Propose, Policy: policy, Grant: ptr(grtA), Classes: classes}, b, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewProposalAndClose(t *testing.T) {
	p := proposal(t, Human)
	if _, err := contract.Encode("records/proposal", p); err != nil {
		t.Fatal(err)
	}
	want, _ := BindingDigest(bound())
	if p.BindingDigest != want || p.Status != types.ProposalStatusOpen || p.Policy != types.ProposalPolicyHuman {
		t.Fatalf("%+v", p)
	}
	if _, err := NewProposal(prpID, Decision{Disposition: Allow}, bound(), now); err == nil {
		t.Error("a proposal from an allow")
	}
	confirmed, err := Close(p, types.ProposalStatusConfirmed)
	if err != nil || confirmed.Status != types.ProposalStatusConfirmed || p.Status != types.ProposalStatusOpen {
		t.Fatalf("close: %v, %+v", err, confirmed)
	}
	if got := refusalOf(t, func() error { _, err := Close(confirmed, types.ProposalStatusRejected); return err }()); got != vocab.ReasonProposalNotOpen {
		t.Errorf("a closed proposal closed again: %s", got.String())
	}
	if _, err := Close(p, types.ProposalStatusOpen); err == nil {
		t.Error("closing to open")
	}
}

var operator = actor(opID, types.ActorKindOperator)

func challenge(t *testing.T, p types.Proposal) types.Challenge {
	t.Helper()
	c, err := IssueChallenge(chlID, p, []types.Actor{operator}, bytes.NewReader(bytes.Repeat([]byte{7}, 32)), now)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChallenges(t *testing.T) {
	p := proposal(t, Human)
	c := challenge(t, p)
	if _, err := contract.Encode("records/challenge", c); err != nil {
		t.Fatal(err)
	}
	if c.Nonce != strings.Repeat("07", 32) || c.BindingDigest != p.BindingDigest || c.ExpiresAt != types.Time(now.Add(ChallengeTTL).Format(time.RFC3339)) {
		t.Fatalf("%+v", c)
	}
	revokedOp := operator
	revokedOp.RevokedAt = ptr(types.Time("2026-10-05T11:00:00Z"))
	for name, issue := range map[string]func() error{
		"under a review policy": func() error {
			_, err := IssueChallenge(chlID, proposal(t, IndependentReview), []types.Actor{operator}, bytes.NewReader(make([]byte, 32)), now)
			return err
		},
		"for a supervisor": func() error {
			_, err := IssueChallenge(chlID, p, []types.Actor{actor(supID, types.ActorKindSupervisor)}, bytes.NewReader(make([]byte, 32)), now)
			return err
		},
		"for a revoked operator": func() error {
			_, err := IssueChallenge(chlID, p, []types.Actor{revokedOp}, bytes.NewReader(make([]byte, 32)), now)
			return err
		},
		"for nobody": func() error {
			_, err := IssueChallenge(chlID, p, nil, bytes.NewReader(make([]byte, 32)), now)
			return err
		},
		"after expiry": func() error {
			_, err := IssueChallenge(chlID, p, []types.Actor{operator}, bytes.NewReader(make([]byte, 32)), now.Add(time.Hour))
			return err
		},
	} {
		if err := issue(); err == nil {
			t.Errorf("a challenge %s", name)
		}
	}
	if _, err := IssueChallenge(chlID, p, []types.Actor{operator}, bytes.NewReader(make([]byte, 8)), now); err == nil {
		t.Error("a nonce from a short random source")
	}

	used, err := ConsumeChallenge(c, p, operator, now)
	if err != nil || used.UsedAt == nil || c.UsedAt != nil {
		t.Fatalf("consume: %v", err)
	}
	other := p
	other.BindingDigest = dgB
	for name, consume := range map[string]func() error{
		"twice":               func() error { _, err := ConsumeChallenge(used, p, operator, now); return err },
		"after its expiry":    func() error { _, err := ConsumeChallenge(c, p, operator, now.Add(ChallengeTTL)); return err },
		"for another binding": func() error { _, err := ConsumeChallenge(c, other, operator, now); return err },
		"by another operator": func() error {
			_, err := ConsumeChallenge(c, p, actor(sup2ID, types.ActorKindOperator), now)
			return err
		},
		"by a revoked operator": func() error { _, err := ConsumeChallenge(c, p, revokedOp, now); return err },
		"by a supervisor": func() error {
			_, err := ConsumeChallenge(c, p, actor(opID, types.ActorKindSupervisor), now)
			return err
		},
	} {
		if got := refusalOf(t, consume()); got != vocab.ReasonConfirmationRefused {
			t.Errorf("consumed %s: %s", name, got.String())
		}
	}
	closed, _ := Close(p, types.ProposalStatusRejected)
	if got := refusalOf(t, func() error { _, err := ConsumeChallenge(c, closed, operator, now); return err }()); got != vocab.ReasonProposalNotOpen {
		t.Errorf("a challenge for a closed proposal: %s", got.String())
	}
}

type blobs map[string][]byte

func (b blobs) Has(d string) (bool, error) { _, ok := b[d]; return ok, nil }
func (b blobs) Open(d string) (io.ReadCloser, error) {
	return nil, errors.New("no such blob")
}

func reviewContract() types.InstrumentContract {
	return types.InstrumentContract{Name: "review-panel", Version: "1.0.0", Digest: dgA, SubjectType: types.InstrumentContractSubjectTypeProposal,
		RequiredPredicates: []types.InstrumentContractRequiredPredicatesItem{{Name: "findings", EvidenceRequired: []string{}}},
		OptionalPredicates: []string{}, Preconditions: []string{}, ArtifactClasses: map[string]types.InstrumentContractArtifactClassesValue{}}
}

// review is a review receipt about p's binding with its derived fields.
func review(t *testing.T, p types.Proposal, binding types.Digest, outcome vocab.Outcome) types.Receipt {
	t.Helper()
	reason := vocab.ReasonCompletedClean
	if outcome == vocab.Found {
		reason = vocab.ReasonCompletedWithFindings
	}
	r := types.Receipt{ID: "rcp_01m3250v57fhm9v1z9xd1wk8gw", Kind: types.ReceiptKindReview,
		Instrument: types.InstrumentRef{Name: "review-panel", Version: "1.0.0", Digest: dgA},
		Subject:    types.Subject{Variant: &types.ProposalSubject{Type: "proposal", Proposal: p.ID, BindingDigest: binding}},
		Provenance: types.ReceiptProvenance{Command: "cmd_01m3250vz2ezn81cgvbkcn17jd", Calls: []types.CallID{}, ModelFamilies: []string{"gpt"},
			Contributors: []types.ActorID{}}, ProducedAt: "2026-10-05T11:00:00Z",
		Predicates: []types.Predicate{{Name: "findings", Outcome: types.Outcome(outcome), Reason: reason, Detail: "x", Evidence: []types.PredicateEvidenceItem{}}}}
	d, err := receipt.Derive(reviewContract(), r, blobs{})
	if err != nil {
		t.Fatal(err)
	}
	r.Outcome, r.Reason, r.Detail, r.Verdict, r.ConclusionsDigest = types.Outcome(d.Outcome), d.Reason, d.Detail, d.Verdict, d.ConclusionsDigest
	return r
}

// 04: a review confirms only this proposal's binding, independently, GO.
func TestConfirmByReview(t *testing.T) {
	p := proposal(t, IndependentReview)
	claude := []string{"claude"}
	if err := ConfirmByReview(p, review(t, p, p.BindingDigest, vocab.Empty), reviewContract(), blobs{}, claude, now); err != nil {
		t.Fatalf("an independent GO: %v", err)
	}
	if err := ConfirmByReview(p, review(t, p, dgB, vocab.Empty), reviewContract(), blobs{}, claude, now); err == nil {
		t.Error("a GO for another binding")
	}
	if err := ConfirmByReview(p, review(t, p, p.BindingDigest, vocab.Empty), reviewContract(), blobs{}, []string{"gpt"}, now); err == nil {
		t.Error("a review by the proposer's family")
	}
	if got := refusalOf(t, ConfirmByReview(p, review(t, p, p.BindingDigest, vocab.Found), reviewContract(), blobs{}, claude, now)); got != vocab.ReasonConfirmationRefused {
		t.Errorf("a NO-GO: %s", got.String())
	}
	forged := review(t, p, p.BindingDigest, vocab.Found)
	goVerdict := types.ReceiptVerdictValueGo
	forged.Verdict = &goVerdict
	if err := ConfirmByReview(p, forged, reviewContract(), blobs{}, claude, now); !errors.Is(err, receipt.ErrInvalid) {
		t.Errorf("a recorded GO the derivation does not give: %v", err)
	}
	human := proposal(t, Human)
	if got := refusalOf(t, ConfirmByReview(human, review(t, human, human.BindingDigest, vocab.Empty), reviewContract(), blobs{}, claude, now)); got != vocab.ReasonConfirmationRefused {
		t.Errorf("a review for a human proposal: %s", got.String())
	}
}

// 04: revalidation at apply invalidates, never applies, a proposal whose
// state, grant, policy or confirmer changed.
func TestRevalidate(t *testing.T) {
	reg := registry(t)
	g := grant(grtA, on(prjA), "profile.adopt")
	p, _ := Close(proposal(t, Human), types.ProposalStatusConfirmed)
	req := supRequest("profile.adopt", []string{"policy.widen_destinations"}, g)
	state := types.ExpectedState{Version: ptr(int64(4))}
	if err := Revalidate(reg, p, &operator, req, state); err != nil {
		t.Fatalf("an unchanged proposal: %v", err)
	}
	if got := refusalOf(t, Revalidate(reg, proposal(t, Human), &operator, req, state)); got != vocab.ReasonProposalNotOpen {
		t.Errorf("an unconfirmed proposal: %s", got.String())
	}
	if got := refusalOf(t, Revalidate(reg, p, &operator, req, types.ExpectedState{Version: ptr(int64(5))})); got != vocab.ReasonStaleExpectedState {
		t.Errorf("a moved object: %s", got.String())
	}
	revokedOp := operator
	revokedOp.RevokedAt = ptr(types.Time("2026-10-05T11:00:00Z"))
	tighter := g
	tighter.ConfirmationOverrides = map[string]types.GrantConfirmationOverridesValue{"profile.adopt": "human"}
	strict := grant(grtB, on(prjA), "profile.adopt")
	for name, c := range map[string]struct {
		grant     *types.GrantID
		confirmer *types.Actor
		req       Request
	}{
		"a revoked grant":     {ptr(grtA), &operator, func() Request { r := req; r.Revoked = map[types.GrantID]bool{grtA: true}; return r }()},
		"another proposer":    {ptr(grtA), &operator, func() Request { r := req; r.Actor = actor(sup2ID, types.ActorKindSupervisor); return r }()},
		"another action":      {ptr(grtA), &operator, func() Request { r := req; r.Action = "task.create"; return r }()},
		"a substitute grant":  {ptr(grtA), &operator, supRequest("profile.adopt", []string{"policy.widen_destinations"}, strict)},
		"no longer sensitive": {ptr(grtA), &operator, supRequest("profile.adopt", nil, g)},
		"a revoked confirmer": {ptr(grtA), &revokedOp, req},
		"no confirmer":        {ptr(grtA), nil, req},
	} {
		if got := refusalOf(t, Revalidate(reg, p, c.confirmer, c.req, state)); got != vocab.ReasonProposalInvalidated {
			t.Errorf("%s: %s", name, got.String())
		}
	}
	// Isolated: only the proposer, or only the action, differs.
	peer := grant(grtA, on(prjA), "profile.adopt")
	peer.Actor = sup2ID
	byPeer := supRequest("profile.adopt", []string{"policy.widen_destinations"}, peer)
	byPeer.Actor = actor(sup2ID, types.ActorKindSupervisor)
	if got := refusalOf(t, Revalidate(reg, p, &operator, byPeer, state)); got != vocab.ReasonProposalInvalidated {
		t.Errorf("another proposer under an equal grant: %s", got.String())
	}
	both := grant(grtA, on(prjA), "profile.adopt", "task.create")
	other := supRequest("task.create", []string{"policy.widen_destinations"}, both)
	if got := refusalOf(t, Revalidate(reg, p, &operator, other, state)); got != vocab.ReasonProposalInvalidated {
		t.Errorf("another action under the same grant: %s", got.String())
	}
	// An operator's proposal has no grant: a refusal at apply (here, the
	// proposer was revoked) invalidates it all the same.
	opBound := bound()
	opBound.ProposedBy, opBound.Classes = opID, []string{"knowledge.export_framework_prose"}
	opP, err := NewProposal(prpID, Decision{Disposition: Propose, Policy: Human, Classes: opBound.Classes}, opBound, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	opP, _ = Close(opP, types.ProposalStatusConfirmed)
	opReq := Request{Actor: operator, Action: "profile.adopt", Project: ptr(prjA), Classes: opBound.Classes, Now: now}
	if err := Revalidate(reg, opP, &operator, opReq, state); err != nil {
		t.Fatalf("an operator's export at apply: %v", err)
	}
	// The registry no longer binds that class to every actor kind: the
	// same change now needs no confirmation, so the proposal is stale.
	relaxed := registry(t)
	cd := relaxed.ChangeClasses["knowledge.export_framework_prose"]
	cd.EveryActorKind = false
	relaxed.ChangeClasses["knowledge.export_framework_prose"] = cd
	if got := refusalOf(t, Revalidate(relaxed, opP, &operator, opReq, state)); got != vocab.ReasonProposalInvalidated {
		t.Errorf("a change that no longer needs confirmation: %s", got.String())
	}
	gone := operator
	gone.RevokedAt = ptr(types.Time("2026-10-05T11:00:00Z"))
	opReq.Actor = gone
	if got := refusalOf(t, Revalidate(reg, opP, &operator, opReq, state)); got != vocab.ReasonProposalInvalidated {
		t.Errorf("a revoked proposer at apply: %s", got.String())
	}
	// Security review: the re-decision is bound to the proposal's project
	// and change, not to whatever the caller passes.
	elsewhere := grant(grtA, on(prjA, prjB), "profile.adopt")
	moved := supRequest("profile.adopt", []string{"policy.widen_destinations"}, elsewhere)
	moved.Project = ptr(prjB)
	if got := refusalOf(t, Revalidate(reg, p, &operator, moved, state)); got != vocab.ReasonProposalInvalidated {
		t.Errorf("another project: %s", got.String())
	}
	reclassed := supRequest("profile.adopt", []string{"policy.widen_data"}, g)
	if got := refusalOf(t, Revalidate(reg, p, &operator, reclassed, state)); got != vocab.ReasonProposalInvalidated {
		t.Errorf("another change at the same policy: %s", got.String())
	}
	review, _ := Close(proposalOf(t, IndependentReview, []string{"install.self"}), types.ProposalStatusConfirmed)
	reviewReq := supRequest("profile.adopt", []string{"install.self"}, g)
	if err := Revalidate(reg, review, nil, reviewReq, state); err != nil {
		t.Errorf("a review-confirmed proposal needs no human confirmer: %v", err)
	}
	tight := supRequest("profile.adopt", []string{"install.self"}, tighter)
	if got := refusalOf(t, Revalidate(reg, review, nil, tight, state)); got != vocab.ReasonProposalInvalidated {
		t.Errorf("a tighter policy now: %s", got.String())
	}
}
