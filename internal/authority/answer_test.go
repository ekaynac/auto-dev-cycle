// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The real authority is the graph's (compile-time check).
var _ graph.Authority = authority.Answers{}

// answers is the boundary with the production input.answer handler, an
// asking node of project A and the owed input it asked for, waiting on the
// operator.
type answers struct {
	*boundary
	asker types.NodeID
	owed  types.OwedInput
}

func openAnswers(t *testing.T) *answers {
	b := &answers{boundary: openBoundary(t)}
	b.az.Handlers[authority.InputAnswerAction] = authority.InputAnswerHandler{}
	b.asker = b.node(prjA, nil, nil)
	b.owed = types.OwedInput{ID: types.OwedInputID(newID(t, "own")), Project: prjA, RequestedBy: b.asker, Owner: b.op.Actor(),
		Kind: types.OwedInputKindRuling, Question: "Is the vendor licence compatible?", Revision: 1,
		Subject: types.ObjectRef{Type: "task", ID: newID(t, "tsk"), Version: ptrTo(int64(0))}}
	b.seed(b.op, owe(b.owed))
	return b
}

func ptrTo[T any](v T) *T { return &v }

// node records a plan node of project, the round 1 node or pred's
// successor with inputs, and returns it.
func (b *answers) node(project types.ProjectID, pred *types.NodeID, inputs []types.InputSpec) (id types.NodeID) {
	b.t.Helper()
	b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		if pred != nil {
			evs, err := graph.Successor(tx, *pred, inputs, authority.Answers{})
			if err == nil {
				var g types.GraphEvent
				err = json.Unmarshal(evs[len(evs)-1].Payload, &g)
				id = g.Variant.(*types.NodeRecorded).Node.ID
			}
			return evs, err
		}
		id = types.NodeID(newID(b.t, "nod"))
		return graph.Record(types.NodeRecorded{Task: types.TaskID(newID(b.t, "tsk")), Project: project, Node: types.Node{ID: id, Kind: types.NodeKindPlan,
			DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`), Inputs: []types.InputSpec{}, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}})
	})
	return id
}

func owe(o types.OwedInput) applyFn {
	return func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.RecordOwed(o) }
}

// revise records the owed input's next revision.
func (b *answers) revise() {
	b.t.Helper()
	b.owed.Revision++
	b.owed.Question += " (revised)"
	b.seed(b.op, owe(b.owed))
}

func answerScope(owed types.OwedInputID, rev int64) []byte {
	return must(json.Marshal(map[string]any{"owed_input": owed, "owed_revision": rev, "answer": map[string]any{"ruling": "compatible"}}))
}

// answer submits input.answer as p under key, expecting version expect,
// with its logic; ran reports whether the logic ran.
func (b *answers) answer(p identity.Principal, key string, raw []byte, expect int64) (out eventstore.Committed, ran bool, err error) {
	c := p.Command(newID(b.t, "cmd"), authority.InputAnswerAction, key, raw, nil)
	c.Expect = version(expect)
	decide := authority.InputAnswer(c)
	out, err = b.s.Submit(context.Background(), c, func(tx *eventstore.Tx) (eventstore.Decision, error) {
		ran = true
		return decide(tx)
	})
	return out, ran, err
}

func (b *answers) record() (r authority.OwedRecord) {
	b.t.Helper()
	b.view(func(tx *eventstore.Tx) error {
		raw, _, err := tx.Get(authority.OwedProjection, string(b.owed.ID))
		if err == nil {
			err = json.Unmarshal(raw, &r)
		}
		return err
	})
	return r
}

// declaring is a node answering the owed input at rev: the asker's
// successor.
func (b *answers) declaring(rev int64) types.NodeRecorded { return declaringOf(b.owed.ID, rev) }

func declaringOf(owed types.OwedInputID, rev int64) types.NodeRecorded {
	return types.NodeRecorded{Project: prjA, Node: types.Node{ID: "nod_x", Inputs: []types.InputSpec{{Name: "ruling",
		Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: owed, Revision: rev}}}}}}
}

// answered is the authority's answer to the owed input at rev.
func (b *answers) answered(rev int64) (types.ApprovalID, bool) { return b.answeredOf(b.owed.ID, rev) }

func (b *answers) answeredOf(owed types.OwedInputID, rev int64) (apr types.ApprovalID, ok bool) {
	b.t.Helper()
	b.view(func(tx *eventstore.Tx) (err error) {
		apr, ok, err = authority.Answers{}.Answer(tx, declaringOf(owed, rev), owed, rev)
		return err
	})
	return apr, ok
}

// revokedRef: the authority refuses the Ref to apr, answering owed at
// rev, as authority_revoked.
func (b *answers) revokedRef(owed types.OwedInputID, rev int64, apr types.ApprovalID) bool {
	b.t.Helper()
	var err error
	b.view(func(tx *eventstore.Tx) error {
		_, err = authority.Answers{}.Approval(tx, declaringOf(owed, rev), owed, rev, apr)
		return nil
	})
	var r graph.ErrRevalidated
	return errors.As(err, &r) && r.Reason == vocab.ReasonAuthorityRevoked
}

// approvalOf is the approval apr as recorded.
func (b *answers) approvalOf(apr types.ApprovalID) (a types.Approval) {
	b.t.Helper()
	b.view(func(tx *eventstore.Tx) error {
		raw, _, err := tx.Get(authority.OwedProjection, "approval/"+string(apr))
		if err == nil {
			err = json.Unmarshal(raw, &a)
		}
		return err
	})
	return a
}

// The operator answers the owed input at its revision, its --expect: the
// command records approval.recorded answering it, bound to that revision;
// the authority gives that approval, its owner and the owed input as its
// asker's; a repeat replays; the answers rebuild.
func TestInputAnswer(t *testing.T) {
	b := openAnswers(t)
	raw := answerScope(b.owed.ID, 1)
	out, ran, err := b.answer(b.op, "k", raw, 1)
	if err != nil || !ran || out.Disposition != eventstore.DispositionApplied || out.LastSeq != out.FirstSeq {
		t.Fatalf("the answer: %+v %v", out, err)
	}
	ev := b.lastEvent()
	var apr types.Approval
	if err := json.Unmarshal(ev.Payload, &apr); err != nil || ev.Kind != authority.ApprovalRecorded || contract.Validate("records/approval", ev.Payload) != nil {
		t.Fatalf("the approval %s: %v", ev.Payload, err)
	}
	want := authority.AnswerApprovalOf(b.record(), types.Actor{ID: b.op.Actor(), Kind: types.ActorKindOperator}, nil, raw, apr.ID, apr.At)
	if w, g := must(json.Marshal(want)), must(json.Marshal(apr)); string(w) != string(g) || *ev.Project != string(prjA) {
		t.Fatalf("the approval:\n got %s\nwant %s", g, w)
	}
	if r := b.record(); r.Answer == nil || r.Answer.Approval != apr.ID || r.Answer.Revision != 1 || r.Answer.Actor != b.op.Actor() {
		t.Fatalf("the owed input: %+v", r)
	}
	if got, ok := b.answered(1); !ok || got != apr.ID {
		t.Fatalf("the authority's answer: %s %t", got, ok)
	}
	b.view(func(tx *eventstore.Tx) error {
		owner, err := authority.Answers{}.Owner(tx, b.declaring(1), b.owed.ID)
		asker := types.NodeRecorded{Project: prjA, Node: types.Node{ID: b.asker}}
		owed, err2 := authority.Answers{}.Owed(tx, asker)
		ref, err3 := authority.Answers{}.Approval(tx, b.declaring(1), b.owed.ID, 1, apr.ID)
		if errors.Join(err, err2, err3) != nil || owner != b.op.Actor() || len(owed) != 1 || owed[0].OwedInput != b.owed.ID || owed[0].Revision != 1 ||
			ref.Type != types.RefTypeApproval || ref.ID != string(apr.ID) || *ref.Scope.Project != prjA {
			t.Fatalf("the authority: %s %+v %+v %v", owner, owed, ref, errors.Join(err, err2, err3))
		}
		if _, err := (authority.Answers{}).Approval(tx, b.declaring(2), b.owed.ID, 2, apr.ID); err == nil {
			t.Error("the approval's Ref at another revision")
		}
		other := b.declaring(1)
		other.Project = prjB
		if _, ok, err := (authority.Answers{}).Answer(tx, other, b.owed.ID, 1); err != nil || ok {
			t.Errorf("a node of project B took A's answer: %t %v", ok, err)
		}
		return nil
	})
	if again, _, err := b.answer(b.op, "k", raw, 1); err != nil || !again.Replayed || again.CommandID != out.CommandID {
		t.Errorf("a repeat: %+v %v", again, err)
	}
	b.rebuilds()
}

// F (an answer at a stale revision): an answer binding a revision other
// than the owed input's current one is stale_expected_state, recording
// nothing: revision 0 (X6's first answer), and revision 1 once the
// question moved to revision 2. The answer to revision 1 then answers
// nothing at revision 2, and does not answer revision 1 either.
func TestInputAnswerStaleRevision(t *testing.T) {
	b := openAnswers(t)
	h := b.head()
	if _, ran, err := b.answer(b.op, "r0", answerScope(b.owed.ID, 0), 1); !errors.Is(err, eventstore.ErrStaleExpectedState) || !ran || b.head() != h {
		t.Fatalf("an answer at revision 0: %v (logic ran: %t)", err, ran)
	}
	if out, _, err := b.answer(b.op, "r1", answerScope(b.owed.ID, 1), 1); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("the answer at revision 1: %v", err)
	}
	apr1, _ := b.answered(1)
	if b.revokedRef(b.owed.ID, 1, apr1) {
		t.Fatal("the Ref to the answer in force is refused")
	}
	b.revise()
	if !b.revokedRef(b.owed.ID, 1, apr1) || !b.revokedRef(b.owed.ID, 2, apr1) {
		t.Error("a Ref to the answer to revision 1, no longer in force")
	}
	if _, ok := b.answered(1); ok {
		t.Error("an answer to revision 1 answers once the question is at revision 2")
	}
	if _, ok := b.answered(2); ok {
		t.Error("an answer to revision 1 answers revision 2")
	}
	h = b.head()
	if _, ran, err := b.answer(b.op, "r1-again", answerScope(b.owed.ID, 1), 2); !errors.Is(err, eventstore.ErrStaleExpectedState) || !ran || b.head() != h {
		t.Fatalf("an answer at the stale revision 1: %v (logic ran: %t)", err, ran)
	}
	if out, _, err := b.answer(b.op, "r2", answerScope(b.owed.ID, 2), 2); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("the answer at revision 2: %v", err)
	}
	if _, ok := b.answered(2); !ok {
		t.Error("the answer to revision 2 does not answer it")
	}
	if _, ok := b.answered(1); ok {
		t.Error("the answer to revision 2 answers the stale revision 1")
	}
	b.rebuilds()
}

// F, at the fold: an approval.recorded event that skips the logic and
// binds a stale revision is refused from the log alone; so is one binding
// the current revision of an owed input already answered.
func TestAnswerFoldRefusesStaleRevision(t *testing.T) {
	b := openAnswers(t)
	forged := func(rev, scope int64) applyFn {
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			o := authority.OwedRecord{OwedInput: b.owed}
			o.Revision = rev
			a := authority.AnswerApprovalOf(o, types.Actor{ID: b.op.Actor(), Kind: types.ActorKindOperator}, nil, answerScope(b.owed.ID, scope),
				types.ApprovalID(newID(b.t, "apr")), types.Time(tx.Now().Format(time.RFC3339Nano)))
			return authority.ApprovalEvent(a)
		}
	}
	b.revise()
	for name, apply := range map[string]applyFn{"revision 1 of 2": forged(1, 1), "revision 3 of 2": forged(3, 3), "a scope at 1, bound at 2": forged(2, 1)} {
		if err := b.try(b.op, apply); err == nil {
			t.Errorf("%s: admitted", name)
		}
	}
	b.seed(b.op, forged(2, 2)) // the current revision, through the same path
	if err := b.try(b.op, forged(2, 2)); err == nil {
		t.Error("a second answer in force admitted")
	}
	b.rebuilds()
}

// Who answers, and what (04: the responder may answer this kind for this
// project): a supervisor without a grant covering input.answer on the
// project is action_not_granted; a granted supervisor answering an
// operator-owned ruling is action_not_granted too, recording nothing; it
// answers an owed input it owns, its approval naming its grant. Revoking
// that grant leaves the answer in force; revoking the actor withdraws it
// (its Ref is then authority_revoked), and the operator answers again at
// the same revision. An owed input answered in force is
// stale_expected_state; a receipt owed input is an invalid scope; an
// unknown one is stale.
func TestInputAnswerRefusals(t *testing.T) {
	b := openAnswers(t)
	var refused *authority.Refusal
	if _, ran, err := b.answer(b.sup, "no-grant", answerScope(b.owed.ID, 1), 1); !errors.As(err, &refused) || refused.Reason != vocab.ReasonActionNotGranted || ran {
		t.Fatalf("an ungranted supervisor: %v", err)
	}
	g := b.supGrant
	g.ID, g.Actions = types.GrantID(newID(t, "grt")), []types.ActionKind{authority.InputAnswerAction}
	b.seed(b.op, createGrant(g))
	h := b.head()
	if _, ran, err := b.answer(b.sup, "not-owner", answerScope(b.owed.ID, 1), 1); !errors.As(err, &refused) || refused.Reason != vocab.ReasonActionNotGranted || !ran || b.head() != h {
		t.Fatalf("a granted supervisor answering an operator's ruling: %v (logic ran: %t)", err, ran)
	}
	mine := b.owed
	mine.ID, mine.Owner = types.OwedInputID(newID(t, "own")), b.sup.Actor()
	b.seed(b.op, owe(mine))
	if out, _, err := b.answer(b.sup, "owner", answerScope(mine.ID, 1), 1); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("a granted supervisor answering its own: %v", err)
	}
	apr, ok := b.answeredOf(mine.ID, 1)
	if a := b.approvalOf(apr); !ok || a.Grant == nil || *a.Grant != g.ID || a.ActorKind != types.ApprovalActorKindSupervisor {
		t.Fatalf("the supervisor's answer: %+v %t", a, ok)
	}
	b.seed(b.op, revokeGrant(g, suiteNow()))
	if got, ok := b.answeredOf(mine.ID, 1); !ok || got != apr || b.revokedRef(mine.ID, 1, apr) {
		t.Fatal("revoking the grant withdrew the answer")
	}
	if _, ran, err := b.answer(b.op, "twice", answerScope(mine.ID, 1), 1); !errors.Is(err, eventstore.ErrStaleExpectedState) || !ran {
		t.Fatalf("an answered owed input: %v", err)
	}
	b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, b.sup.Actor(), tx.Now())
	})
	if _, ok := b.answeredOf(mine.ID, 1); ok || !b.revokedRef(mine.ID, 1, apr) {
		t.Fatal("a revoked actor's answer is in force")
	}
	if out, _, err := b.answer(b.op, "after-revoke", answerScope(mine.ID, 1), 1); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("an answer after the revocation: %v", err)
	}
	receipt := b.owed
	receipt.ID, receipt.Kind, receipt.Revision = types.OwedInputID(newID(t, "own")), types.OwedInputKindReceipt, 1
	b.seed(b.op, owe(receipt))
	h = b.head()
	if _, _, err := b.answer(b.op, "receipt", answerScope(receipt.ID, 1), 1); !errors.Is(err, authority.ErrInvalidScope) || b.head() != h {
		t.Errorf("a receipt owed input: %v", err)
	}
	if _, ran, err := b.answer(b.op, "unknown", answerScope(types.OwedInputID(newID(t, "own")), 1), 1); !errors.Is(err, eventstore.ErrStaleExpectedState) || ran {
		t.Errorf("an unknown owed input: %v", err)
	}
	b.view(func(tx *eventstore.Tx) error {
		owed, err := authority.Answers{}.Owed(tx, types.NodeRecorded{Project: prjA, Node: types.Node{ID: b.asker}})
		if err != nil || len(owed) != 2 || slices.ContainsFunc(owed, func(a types.AnswerSource) bool { return a.OwedInput == receipt.ID }) {
			t.Errorf("a receipt owed input is an answer source: %+v %v", owed, err)
		}
		return nil
	})
	b.rebuilds()
}

// No existence oracle: a supervisor granted input.answer on project A is
// refused an unknown owed input exactly as one of project B, which its
// grants do not cover, before anything runs.
func TestInputAnswerNoExistenceOracle(t *testing.T) {
	b := openAnswers(t)
	g := b.supGrant
	g.ID, g.Actions = types.GrantID(newID(t, "grt")), []types.ActionKind{authority.InputAnswerAction}
	b.seed(b.op, createGrant(g))
	foreign := b.owed
	foreign.ID, foreign.Project, foreign.RequestedBy = types.OwedInputID(newID(t, "own")), prjB, b.node(prjB, nil, nil)
	b.seed(b.op, owe(foreign))
	refusalOf := func(owed types.OwedInputID) *authority.Refusal {
		var r *authority.Refusal
		if _, ran, err := b.answer(b.sup, newID(t, "cmd"), answerScope(owed, 1), 1); !errors.As(err, &r) || ran {
			t.Fatalf("%s: %v (logic ran: %t)", owed, err, ran)
		}
		return r
	}
	unknown, other := refusalOf(types.OwedInputID(newID(t, "own"))), refusalOf(foreign.ID)
	if unknown.Reason != vocab.ReasonActionNotGranted || *unknown != *other {
		t.Errorf("an unknown owed input %+v, a foreign one %+v", unknown, other)
	}
}

// An owed input is admitted only as its asking node's, in that node's
// project, waiting on a live operator or supervisor, at revision 1 or the
// next revision of the same project, node and kind.
func TestOwedInputFold(t *testing.T) {
	b := openAnswers(t)
	other := b.node(prjB, nil, nil)
	bad := map[string]func(*types.OwedInput){
		"another project's node": func(o *types.OwedInput) { o.RequestedBy = other },
		"an unknown node":        func(o *types.OwedInput) { o.RequestedBy = types.NodeID(newID(t, "nod")) },
		"an unknown owner":       func(o *types.OwedInput) { o.Owner = types.ActorID(newID(t, "act")) },
		"a system owner":         func(o *types.OwedInput) { o.Owner = b.sys.Actor() },
		"a new one at 2":         func(o *types.OwedInput) { o.ID, o.Revision = types.OwedInputID(newID(t, "own")), 2 },
		"a revision skipped":     func(o *types.OwedInput) { o.Revision = 3 },
		"a revision repeated":    func(o *types.OwedInput) {},
		"another node":           func(o *types.OwedInput) { o.Revision, o.RequestedBy = 2, b.node(prjA, nil, nil) },
		"another kind":           func(o *types.OwedInput) { o.Revision, o.Kind = 2, types.OwedInputKindData },
	}
	for name, edit := range bad {
		o := b.owed
		edit(&o)
		if err := b.try(b.op, owe(o)); err == nil {
			t.Errorf("%s: admitted", name)
		}
	}
	b.rebuilds()
}

// The real authority under graph evaluation (13 "Evaluation"; X6): the
// asker's successor declaring the answer holds awaiting_input on
// <owed>@1, owned by the owed input's owner; the answer at revision 0 is
// refused and the hold stays; the answer at revision 1 clears it and
// claims the successor in the same evaluation; the successor's controller
// context holds exactly that approval's Ref.
func TestAnswersUnderEvaluation(t *testing.T) {
	b := openAnswers(t)
	ask := &types.AnswerSource{Type: "answer", OwedInput: b.owed.ID, Revision: 1}
	next := b.node(prjA, &b.asker, []types.InputSpec{{Name: "ruling", Source: types.InputSpecSource{Variant: ask}}})
	none := func(types.Node, types.InputSpec) (*receipt.Requirement, error) { return nil, nil }
	env := graph.Env{Need: none, Authority: authority.Answers{}}
	evaluate := func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Evaluate(tx, []types.NodeID{next}, env)
	}
	state := func() (s struct{ Holds []types.NodeHold }) {
		b.view(func(tx *eventstore.Tx) error {
			held, err := graph.Held(tx)
			for _, h := range held {
				if h.Node == next {
					s.Holds = h.Holds
				}
			}
			return err
		})
		return s
	}
	b.seed(b.op, evaluate)
	holds := state().Holds
	if len(holds) != 1 || holds[0].Hold != "awaiting_input" || holds[0].SignalInstance != string(b.owed.ID)+"@1" ||
		holds[0].OwnerRef == nil || *holds[0].OwnerRef != string(b.op.Actor()) {
		t.Fatalf("the successor's holds: %+v", holds)
	}
	if _, _, err := b.answer(b.op, "r0", answerScope(b.owed.ID, 0), 1); !errors.Is(err, eventstore.ErrStaleExpectedState) {
		t.Fatalf("the answer at revision 0: %v", err)
	}
	b.seed(b.op, evaluate)
	if len(state().Holds) != 1 {
		t.Fatal("the hold was cleared without an answer")
	}
	if _, _, err := b.answer(b.op, "r1", answerScope(b.owed.ID, 1), 1); err != nil {
		t.Fatal(err)
	}
	apr, _ := b.answered(1)
	b.seed(b.op, evaluate)
	if len(state().Holds) != 0 || b.lastEvent().Kind != graph.NodeClaimed {
		t.Fatalf("after the answer: holds %+v, last %s", state().Holds, b.lastEvent().Kind)
	}
	b.view(func(tx *eventstore.Tx) error {
		ctx, err := graph.Context(tx, next, env)
		if err != nil || len(ctx.Included) != 1 || ctx.Included[0].ID != string(apr) || ctx.Included[0].Type != types.RefTypeApproval {
			t.Errorf("the successor's context: %+v %v", ctx, err)
		}
		return nil
	})
	b.rebuilds()
}

// M1 P6, adcd's composition: the graph fold reads authority.Answers. Before
// the answer, a raw claim of the successor on an approval that answers
// nothing is refused authority_revoked, and, once the evaluation holds it
// on awaiting_input, a raw clear of that wait is refused; once answered,
// the evaluation clears the wait and claims, and the log rebuilds.
func TestAnswerFoldsReadTheAuthority(t *testing.T) {
	b := openAnswers(t)
	ask := &types.AnswerSource{Type: "answer", OwedInput: b.owed.ID, Revision: 1}
	next := b.node(prjA, &b.asker, []types.InputSpec{{Name: "ruling", Source: types.InputSpecSource{Variant: ask}}})
	none := func(types.Node, types.InputSpec) (*receipt.Requirement, error) { return nil, nil }
	env := graph.Env{Need: none, Authority: authority.Answers{}}
	try := func(apply applyFn) error {
		cmd := newID(t, "cmd")
		_, err := b.s.SubmitApplied(context.Background(), b.op.Command(cmd, "test.seed", cmd, []byte(`{}`), nil), eventstore.Applies(
			func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				evs, err := apply(tx)
				return evs, json.RawMessage(`{}`), err
			}))
		return err
	}
	var stale graph.ErrRevalidated
	if err := try(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Claim(tx, next, map[string]graph.Resolved{"ruling": {Approval: types.ApprovalID(newID(t, "apr")), Revision: 1}}, 0)
	}); !errors.As(err, &stale) || stale.Reason != vocab.ReasonAuthorityRevoked {
		t.Errorf("a raw claim before the answer: %v", err)
	}
	b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Evaluate(tx, []types.NodeID{next}, env)
	})
	var hold types.NodeHold
	b.view(func(tx *eventstore.Tx) error {
		held, err := graph.Held(tx)
		for _, h := range held {
			if h.Node == next {
				hold = h.Holds[0]
			}
		}
		return err
	})
	if err := try(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Clear(tx, next, hold.ID, hold.SignalInstance)
	}); err == nil {
		t.Error("a raw clear before the answer")
	}
	if _, _, err := b.answer(b.op, "r1", answerScope(b.owed.ID, 1), 1); err != nil {
		t.Fatal(err)
	}
	b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Evaluate(tx, []types.NodeID{next}, env)
	})
	if b.lastEvent().Kind != graph.NodeClaimed {
		t.Fatalf("after the answer: %s", b.lastEvent().Kind)
	}
	b.rebuilds()
}

// oneFamily is a registry in which every executor is of the claude family.
type oneFamily struct{}

func (oneFamily) Family(types.ExecutorID) string { return "claude" }

// Under the real authority a question never leaves its asker's lineage
// (recordOwed keeps requested_by across revisions), so a supersession
// neither removes nor moves an answer: both are refused, "has not moved"
// (M1 P6, review L3).
func TestSupersedeMovingUnderTheAuthority(t *testing.T) {
	b := openAnswers(t)
	task := types.TaskID(newID(t, "tsk"))
	member := func(exe string) json.RawMessage {
		return json.RawMessage(`[{"executor":"` + exe + `","family":"claude"}]`)
	}
	const exeA, exeB = "exe_01m3250v9xft08ssr4ygq6g2np", "exe_01m3250vkwffc979nzjw0g2sf9"
	record := func(kind types.NodeKind, params string, inputs []types.InputSpec) types.NodeID {
		id := types.NodeID(newID(t, "nod"))
		b.seed(b.op, func(*eventstore.Tx) ([]eventstore.Event, error) {
			return graph.RecordReview(types.NodeRecorded{Task: task, Project: prjA, Node: types.Node{ID: id, Kind: kind, DefinitionVersion: "1", Round: 1,
				Params: json.RawMessage(params), Inputs: inputs, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}, oneFamily{})
		})
		return id
	}
	requirement := func(rev, exe string) types.NodeID {
		return record(types.NodeKindReviewRequirement, `{"subject":"candidate","roster":`+string(member(exe))+`,"definition_version":"1","revision":`+rev+`}`, []types.InputSpec{})
	}
	req, next := requirement("1", exeA), requirement("2", exeB)
	first := record(types.NodeKindReview, `{"subject":"candidate","roster":`+string(member(exeA))+`}`, []types.InputSpec{{Name: "required",
		Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: req, ResultKind: types.ResultKindReviewRequirement}}}})
	o, other := b.owed, b.owed
	o.ID, o.RequestedBy = types.OwedInputID(newID(t, "own")), first
	other.ID, other.RequestedBy = types.OwedInputID(newID(t, "own")), first
	b.seed(b.op, owe(o))
	b.seed(b.op, owe(other))
	second := b.node(prjA, &first, []types.InputSpec{{Name: "ruling", Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: o.ID, Revision: 1}}}})
	for name, move := range map[string]*types.AnswerSource{"a removal": nil, "a move": {Type: "answer", OwedInput: other.ID, Revision: 1}} {
		b.view(func(tx *eventstore.Tx) error {
			_, err := graph.SupersedeMoving(tx, second, next, nil, graph.Moves{"ruling": move}, authority.Answers{})
			if err == nil || !strings.Contains(err.Error(), "has not moved") {
				t.Errorf("%s: %v", name, err)
			}
			return nil
		})
	}
}

// The fold binds the grant from the log alone: a supervisor's answer
// names its own grant covering input.answer on the project, in force at
// the answer; an operator's none. Refused: a supervisor's answer with no
// grant, under another actor's grant, or under its grant covering only
// note.add; an operator's with a grant.
func TestAnswerFoldBindsGrant(t *testing.T) {
	b := openAnswers(t)
	mine := b.owed
	mine.ID, mine.Owner = types.OwedInputID(newID(t, "own")), b.sup.Actor()
	b.seed(b.op, owe(mine))
	forged := func(by identity.Principal, kind types.ActorKind, o types.OwedInput, grant *types.GrantID) applyFn {
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			a := authority.AnswerApprovalOf(authority.OwedRecord{OwedInput: o}, types.Actor{ID: by.Actor(), Kind: kind}, grant, answerScope(o.ID, 1),
				types.ApprovalID(newID(b.t, "apr")), types.Time(tx.Now().Format(time.RFC3339Nano)))
			return authority.ApprovalEvent(a)
		}
	}
	answering := b.supGrant
	answering.ID, answering.Actions = types.GrantID(newID(t, "grt")), []types.ActionKind{authority.InputAnswerAction}
	b.seed(b.op, createGrant(answering))
	other := answering
	other.ID, other.Actor = types.GrantID(newID(t, "grt")), b.supervisor().Actor()
	b.seed(b.op, createGrant(other))
	for _, c := range []struct {
		name  string
		by    identity.Principal
		apply applyFn
	}{
		{"a supervisor's, no grant", b.sup, forged(b.sup, types.ActorKindSupervisor, mine, nil)},
		{"a supervisor's, another actor's grant", b.sup, forged(b.sup, types.ActorKindSupervisor, mine, &other.ID)},
		{"a supervisor's, a grant covering only note.add", b.sup, forged(b.sup, types.ActorKindSupervisor, mine, &b.supGrant.ID)},
		{"an operator's, a grant", b.op, forged(b.op, types.ActorKindOperator, b.owed, &answering.ID)},
	} {
		if err := b.try(c.by, c.apply); err == nil {
			t.Errorf("%s: admitted", c.name)
		}
	}
	b.seed(b.sup, forged(b.sup, types.ActorKindSupervisor, mine, &answering.ID))
	b.rebuilds()
}
