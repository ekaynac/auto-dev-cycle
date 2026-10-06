// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// proposalKind records a proposal's new state; the projector keeps the
// latest under its id.
const proposalKind = "fixture.proposal/v1"

// writerStep maps a command's outcome to its step, an authority refusal in
// the transaction included.
func writerStep(err error) (Step, error) {
	if a := new(authority.Refusal); errors.As(err, &a) {
		return Step{vocab.Failed, a.Reason.String()}, nil
	}
	return commandStep(err)
}

// concurrentConflictingApprovals: eight confirmations, rejections and
// expiries of one open proposal (a supervisor's protected-document change,
// which authority.Decide proposes for an independent review), each decided
// against its version 1, race through the writer; each runs authority's
// confirm or close on the proposal as the transaction reads it. Exactly one
// applies and the rest are stale: the writer's layer. Authority's own layer
// is shown apart from it: confirming the now-closed proposal directly, with
// no writer, is refused, and so are a confirmation and a rejection decided
// against the new version, which pass the writer's expected-state check. A
// second open proposal confirmed at its expiry instant is refused as
// expired and changes nothing (11 "concurrent conflicting approvals"; 03
// "The writer"; 04 "Proposals and confirmation").
//
// M0 has no proposal.confirm command wired to the writer (no command kind,
// no Authorize hook calling authority): the fixture's command plays it,
// calling authority.ConfirmByReview and authority.Close inside the
// transaction, under an authorizer that allows everything.
func concurrentConflictingApprovals(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	s, done, err := storeWith(ctx, dir, eventstore.Kinds{proposalKind: {Schema: "records/proposal"}},
		func(t *eventstore.Tx, evs []eventstore.Event) error {
			for _, e := range evs {
				if err := t.Put("proposals", e.Subject, e.Payload); err != nil {
					return err
				}
			}
			return nil
		})
	if err != nil {
		return nil, nil, err
	}
	defer done()
	reg, err := vocab.Load()
	if err != nil {
		return nil, nil, err
	}
	sup, _, _, g, err := actors(protectedDocument)
	if err != nil {
		return nil, nil, err
	}
	actor := string(sup.ID)
	// submit runs decide on the proposal the transaction reads (p, before
	// it is recorded) and records its result, as a command decided against
	// version.
	submit := func(p types.Proposal, version int64, decide func(types.Proposal) (types.Proposal, error)) error {
		var idv [2]string
		for i, pre := range []string{"cmd", "evt"} {
			v, err := ids.New(pre)
			if err != nil {
				return err
			}
			idv[i] = v
		}
		id := string(p.ID)
		_, err := s.SubmitApplied(ctx, eventstore.Command{ID: idv[0], Actor: actor, Action: "fixture.command", IdempotencyKey: idv[0], Input: []byte(`{}`),
			Expected: []eventstore.Expectation{{Subject: id, Version: version}}}, eventstore.Applies(

			func(t *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				cur := p
				if raw, ok, err := t.Get("proposals", id); err != nil {
					return nil, nil, err
				} else if ok {
					if cur, err = contract.Decode[types.Proposal]("records/proposal", raw); err != nil {
						return nil, nil, err
					}
				}
				next, err := decide(cur)
				if err != nil {
					return nil, nil, err
				}
				payload, err := contract.Encode("records/proposal", next)
				return []eventstore.Event{{ID: idv[1], Kind: proposalKind, Subject: id, Payload: payload}}, json.RawMessage(`{}`), err
			}))
		return err
	}
	p, err := openProposal(reg, sup, g)
	if err != nil {
		return nil, nil, err
	}
	id := string(p.ID)
	if err := submit(p, 0, func(types.Proposal) (types.Proposal, error) { return p, nil }); err != nil {
		return nil, nil, err
	}
	confirmAt := func(now time.Time) func(types.Proposal) (types.Proposal, error) {
		return func(p types.Proposal) (types.Proposal, error) {
			r, err := newReceipt(types.ReceiptKindReview, reviewPanel(), proposalSubject(p.ID, p.BindingDigest), []string{"claude"},
				pred("findings", vocab.Empty, vocab.ReasonCompletedClean))
			if err == nil {
				r, err = recorded(reviewPanel(), r, nil)
			}
			if err != nil {
				return types.Proposal{}, err
			}
			return authority.ConfirmByReview(p, r, reviewPanel(), nil, []string{"gpt"}, now)
		}
	}
	closeAs := func(to types.ProposalStatus) func(types.Proposal) (types.Proposal, error) {
		return func(p types.Proposal) (types.Proposal, error) { return authority.Close(p, to) }
	}
	confirm := confirmAt(receiptsAt)
	// The contenders confirm, reject and expire it.
	contenders := []func(types.Proposal) (types.Proposal, error){confirm, closeAs(types.ProposalStatusRejected), closeAs(types.ProposalStatusExpired)}
	outcomes := []types.ProposalStatus{types.ProposalStatusConfirmed, types.ProposalStatusRejected, types.ProposalStatusExpired}
	var o observe
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[i] = submit(p, 1, contenders[i%len(contenders)]) })
	}
	wg.Wait()
	winner := -1
	var race []Step
	for i, err := range errs {
		st, err := writerStep(err)
		if err != nil {
			return nil, nil, err
		}
		if st.Reason == vocab.ReasonCompletedClean.String() {
			winner = i
		}
		race = append(race, st)
	}
	slices.SortFunc(race, func(a, b Step) int { return strings.Compare(a.String(), b.String()) })
	o.steps = race
	if winner < 0 {
		return o.steps, append(o.violations, "no approval applied"), nil
	}
	want := outcomes[winner%len(outcomes)]
	got, err := projected(ctx, s, id)
	if err != nil {
		return nil, nil, err
	}
	o.check(got.Status == want, "the proposal is %s, but the command that applied made it %s", got.Status, want)
	_, err = confirm(got) // authority alone, outside the writer
	if err := o.refused(err); err != nil {
		return nil, nil, err
	}
	for _, decide := range contenders[:2] {
		st, err := writerStep(submit(p, 2, decide))
		if err != nil {
			return nil, nil, err
		}
		o.steps = append(o.steps, st)
	}
	h, err := s.Head(ctx)
	if err != nil {
		return nil, nil, err
	}
	o.check(h.Seq == 2, "the log's head is %d, want 2: one approval applied after the proposal", h.Seq)
	// Expiry: a second open proposal confirmed at its expiry instant, by a
	// command decided against its current version, is refused and changes
	// nothing.
	late, err := openProposal(reg, sup, g)
	if err != nil {
		return nil, nil, err
	}
	if err := submit(late, 0, func(types.Proposal) (types.Proposal, error) { return late, nil }); err != nil {
		return nil, nil, err
	}
	at, err := time.Parse(time.RFC3339, string(late.ExpiresAt))
	if err != nil {
		return nil, nil, err
	}
	lateHead, err := s.Head(ctx) // the late proposal's version
	if err != nil {
		return nil, nil, err
	}
	st, err := writerStep(submit(late, lateHead.Seq, confirmAt(at)))
	if err != nil {
		return nil, nil, err
	}
	o.steps = append(o.steps, st)
	still, err := projected(ctx, s, string(late.ID))
	if err != nil {
		return nil, nil, err
	}
	o.check(still.Status == types.ProposalStatusOpen, "the proposal confirmed at its expiry is %s, want open", still.Status)
	if h, err = s.Head(ctx); err != nil {
		return nil, nil, err
	}
	o.check(h == lateHead, "the log's head moved from %d to %d: the confirmation at expiry wrote", lateHead.Seq, h.Seq)
	return o.steps, o.violations, nil
}

// projected is the proposal the store's projection holds.
func projected(ctx context.Context, s *eventstore.Store, id string) (p types.Proposal, err error) {
	err = s.View(ctx, func(t *eventstore.Tx) error {
		raw, ok, err := t.Get("proposals", id)
		if err == nil && !ok {
			err = fmt.Errorf("fixtures: proposal %s is not projected", id)
		}
		if err == nil {
			p, err = contract.Decode[types.Proposal]("records/proposal", raw)
		}
		return err
	})
	return p, err
}

// protectedDocument is the action the proposals are for: a protected
// document change, which a supervisor's grant can only propose for an
// independent review (04; schemas/vocab/actions.yaml).
const protectedDocument = "knowledge.change_protected_document"

// openProposal is the open proposal authority.Decide makes of sup's
// protected-document change under grant g.
func openProposal(reg *vocab.Registry, sup types.Actor, g types.Grant) (types.Proposal, error) {
	prp, err := ids.New("prp")
	if err != nil {
		return types.Proposal{}, err
	}
	project, doc := projectA, "docs/decisions/0001.md"
	d := authority.Decide(reg, authority.Request{Actor: sup, Action: protectedDocument, Project: &project, Grants: []types.Grant{g}, Now: receiptsAt})
	if d.Disposition != authority.Propose || d.Policy != authority.IndependentReview {
		return types.Proposal{}, fmt.Errorf("fixtures: %s was decided %v under %v, not proposed for review", protectedDocument, d.Disposition, d.Policy)
	}
	b := authority.Bound{Action: protectedDocument, Project: &project, Object: types.ObjectRef{Type: "document", ID: doc, Digest: &instrumentDigest},
		ExpectedState: types.ExpectedState{Digest: &instrumentDigest}, Scope: json.RawMessage(`{"path":"` + doc + `"}`), ProposedBy: sup.ID}
	p, err := authority.NewProposal(types.ProposalID(prp), d, b, receiptsAt, receiptsAt.Add(time.Hour))
	if err != nil {
		return types.Proposal{}, fmt.Errorf("fixtures: proposal: %w", err)
	}
	return p, nil
}
