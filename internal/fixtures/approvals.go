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

// concurrentConflictingApprovals: eight confirmations and rejections of
// one open proposal, each decided against its version 1, race through the
// writer; each runs authority's confirm or close on the proposal as the
// transaction reads it. Exactly one applies and the rest are stale: the
// writer's layer. Authority's own layer is shown apart from it: confirming
// the now-closed proposal directly, with no writer, is refused, and so are
// a confirmation and a rejection decided against the new version, which
// pass the writer's expected-state check (11 "concurrent conflicting
// approvals"; 03 "The writer"; 04 "Proposals and confirmation").
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
	actor, err := ids.New("act")
	if err != nil {
		return nil, nil, err
	}
	p, err := openProposal(types.ActorID(actor))
	if err != nil {
		return nil, nil, err
	}
	id := string(p.ID)
	// submit runs decide on the proposal the transaction reads and records
	// its result, as a command decided against version.
	submit := func(version int64, decide func(types.Proposal) (types.Proposal, error)) error {
		var idv [2]string
		for i, pre := range []string{"cmd", "evt"} {
			v, err := ids.New(pre)
			if err != nil {
				return err
			}
			idv[i] = v
		}
		_, err := s.Submit(ctx, eventstore.Command{ID: idv[0], Actor: actor, IdempotencyKey: idv[0], Input: []byte(`{}`),
			Expected: []eventstore.Expectation{{Subject: id, Version: version}}},
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
			})
		return err
	}
	if err := submit(0, func(types.Proposal) (types.Proposal, error) { return p, nil }); err != nil {
		return nil, nil, err
	}
	confirm := func(p types.Proposal) (types.Proposal, error) {
		r, err := newReceipt(types.ReceiptKindReview, reviewPanel(), proposalSubject(p.ID, p.BindingDigest), []string{"claude"},
			pred("findings", vocab.Empty, vocab.ReasonCompletedClean))
		if err == nil {
			r, err = recorded(reviewPanel(), r, nil)
		}
		if err != nil {
			return types.Proposal{}, err
		}
		return authority.ConfirmByReview(p, r, reviewPanel(), nil, []string{"gpt"}, receiptsAt)
	}
	reject := func(p types.Proposal) (types.Proposal, error) {
		return authority.Close(p, types.ProposalStatusRejected)
	}
	var o observe
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[i] = submit(1, []func(types.Proposal) (types.Proposal, error){confirm, reject}[i%2]) })
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
	want := []types.ProposalStatus{types.ProposalStatusConfirmed, types.ProposalStatusRejected}[winner%2]
	got, err := projected(ctx, s, id)
	if err != nil {
		return nil, nil, err
	}
	o.check(got.Status == want, "the proposal is %s, but the command that applied made it %s", got.Status, want)
	_, err = confirm(got) // authority alone, outside the writer
	if err := o.refused(err); err != nil {
		return nil, nil, err
	}
	for _, decide := range []func(types.Proposal) (types.Proposal, error){confirm, reject} {
		st, err := writerStep(submit(2, decide))
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

// openProposal is an open independent_review proposal by proposer.
func openProposal(proposer types.ActorID) (types.Proposal, error) {
	prp, err := ids.New("prp")
	if err != nil {
		return types.Proposal{}, err
	}
	project, four := projectA, int64(4)
	b := authority.Bound{Action: "profile.adopt", Project: &project, Object: types.ObjectRef{Type: "profile", ID: string(projectA), Version: &four},
		ExpectedState: types.ExpectedState{Version: &four}, Scope: json.RawMessage(`{"revision":5}`), ProposedBy: proposer}
	d := authority.Decision{Disposition: authority.Propose, Policy: authority.IndependentReview, Classes: []string{"policy.widen_destinations"}}
	p, err := authority.NewProposal(types.ProposalID(prp), d, b, receiptsAt.Add(time.Hour))
	if err != nil {
		return types.Proposal{}, fmt.Errorf("fixtures: proposal: %w", err)
	}
	return p, nil
}
