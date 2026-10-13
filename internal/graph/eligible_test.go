// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
)

// openFolds is openStore whose graph fold is the M1 fold (Projector, over
// the owed-answer fake) while checked is set, and otherwise a raw writer's:
// the same fold with no answer check, removals admitted.
func openFolds(t *testing.T) (graphStore, *atomic.Bool) {
	t.Helper()
	checked := new(atomic.Bool)
	checked.Store(true)
	state := filepath.Join(t.TempDir(), "state")
	lock, err := eventstore.AcquireLock(state)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	hook := func(tx *eventstore.Tx, evs []eventstore.Event) error {
		if checked.Load() {
			return Projector{Answers: owedanswers.Authority{}}.Project(tx, evs)
		}
		for _, ev := range evs {
			var g types.GraphEvent
			if !slices.Contains(kinds, ev.Kind) {
				continue
			}
			if err := json.Unmarshal(ev.Payload, &g); err != nil {
				return err
			}
			if err := fold(tx, ev, g.Variant, true); err != nil {
				return err
			}
		}
		return nil
	}
	none := func(*eventstore.Tx, eventstore.Command) error { return nil }
	ks := Kinds()
	maps.Copy(ks, owedanswers.Kinds())
	maps.Copy(ks, lease.Kinds())
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: ks, Blobs: blobs,
		Hooks: eventstore.Hooks{Identify: none, Authorize: none, Project: eventstore.ProjectEach(leases(blobs), hook, owedanswers.Project)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); lock.Release() })
	g := graphStore{t: t, s: s, blobs: blobs, conclusions: map[types.Digest]json.RawMessage{}, tree: &struct {
		node types.NodeID
		res  types.ResultID
	}{}, req: new(types.NodeID)}
	g.plan, g.planRes = g.planOf(tskID, prjID)
	return g, checked
}

// refusedOnReplay: the M1 fold refuses raw, naming want; a raw writer
// appends it; and the M1 fold, replaying the log, refuses it again.
func refusedOnReplay(t *testing.T, g graphStore, checked *atomic.Bool, raw logic, want string) {
	t.Helper()
	if err := g.do(raw); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("the M1 fold: %v, want %q", err, want)
	}
	checked.Store(false)
	if err := g.do(raw); err != nil {
		t.Fatalf("a raw writer: %v", err)
	}
	checked.Store(true)
	if _, err := g.s.RebuildProjections(context.Background()); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("the replay: %v, want %q", err, want)
	}
}

// answering is a round 2 review answering the ruling (ownID) at revision
// 2, which its round 1 asks, unanswered.
func (g graphStore) answering() types.NodeID { return g.asking(owes("ruling", ownID)) }

const revoked = "authority_revoked"

// Each raw event that takes an answer its builder would refuse is refused
// by the M1 fold, on append and on replay (M1 P6, F).
func TestFoldRefusesRawClaim(t *testing.T) {
	g, checked := openFolds(t)
	n := g.answering() // unanswered
	refusedOnReplay(t, g, checked, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return claimAny(tx, n, map[string]Resolved{"ruling": {Approval: aprA, Revision: 2}}, 0)
	}, revoked)
}

func TestFoldRefusesRawReuse(t *testing.T) {
	g, checked := openFolds(t)
	env := g.env(gatesOn(treeA, demandA))
	n := g.answering()
	g.must(answer(ownID, actID, ptr(types.ApprovalID(aprA))))
	g.must(evaluate(env, n))
	g.must(commit(n, g.reviewResult(n), env))
	later, err := g.successor(n) // the same inputs, so the same key
	if err != nil {
		t.Fatal(err)
	}
	g.must(answer(ownID, actID, nil)) // withdrawn
	refusedOnReplay(t, g, checked, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		rec, err := loadNode(tx, later)
		resolved := withRequired(tx, later, map[string]Resolved{"ruling": {Approval: aprA, Revision: 2}})
		var key types.ComputationKey
		if err == nil {
			key, err = Key(rec.Recorded.Node, resolved, 1)
		}
		r := types.NodeReused{Type: "node_reused", Node: later, Holder: n, Key: key, Inputs: items(rec.Recorded.Node, resolved)}
		evs, eerr := event(NodeReused, string(later), prjID, &r)
		return evs, errors.Join(err, eerr)
	}, revoked)
}

func TestFoldRefusesRawResult(t *testing.T) {
	g, checked := openFolds(t)
	env := g.env(gatesOn(treeA, demandA))
	n := g.answering()
	g.must(answer(ownID, actID, ptr(types.ApprovalID(aprA))))
	g.must(evaluate(env, n))
	g.must(answer(ownID, actID, nil)) // withdrawn after the claim
	r := g.reviewResult(n)
	refusedOnReplay(t, g, checked, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: n, Result: r}, g.blobs)
	}, revoked)
}

func TestFoldRefusesRawSuccessor(t *testing.T) {
	g, checked := openFolds(t)
	n := g.answering()
	g.must(asked(ownB, g.plan, nil)) // owed by another node, never this lineage
	r := g.recordOf(n)
	r.Node.ID, r.Predecessor, r.Node.Round = g.newNode(types.NodeKindReview).Node.ID, &n, 3
	r.Node.Inputs = append(slices.Clone(r.Node.Inputs), owes("data", ownB))
	refusedOnReplay(t, g, checked, func(*eventstore.Tx) ([]eventstore.Event, error) { return record(r) }, "no ancestor owes")
}

func TestFoldRefusesRawSupersession(t *testing.T) {
	for name, edit := range map[string]func(p *types.ReviewSuperseded){
		"an answer replaced": func(p *types.ReviewSuperseded) { p.Replacement.Inputs[2] = answerAt("ruling", ownC, 2) },
		"an answer removed":  func(p *types.ReviewSuperseded) { p.Replacement.Inputs = slices.Delete(p.Replacement.Inputs, 2, 3) },
	} {
		t.Run(name, func(t *testing.T) {
			g, checked := openFolds(t)
			review, _, next := g.superseding()
			r := g.recordOf(review)
			r.Node.ID = g.newNode(types.NodeKindReview).Node.ID
			first := g.record(r)
			g.must(asked(ownID, first, nil))
			g.must(asked(ownC, first, ptr(types.ApprovalID(aprC))))
			second, err := g.successor(first, owes("ruling", ownID))
			if err != nil {
				t.Fatal(err)
			}
			refusedOnReplay(t, g, checked, edited(second, next, func(p *types.ReviewSuperseded) {
				p.Replacement.Inputs = slices.Clone(p.Replacement.Inputs)
				edit(p)
			}), "has not moved") // the ruling is still asked
		})
	}
}

func TestFoldRefusesRawAnswerWaitEnd(t *testing.T) {
	for name, cause := range map[string]types.HoldClearedCauseVariant{
		"cleared, no answer given": &types.Signalled{Type: "signal", SignalInstance: ownID + "@2"},
		"retired as revised":       &types.Retired{Type: "retired", Reason: types.RetiredReasonRevised},
	} {
		t.Run(name, func(t *testing.T) {
			g, checked := openFolds(t)
			n := g.answering()
			g.must(evaluate(g.env(gatesOn(treeA, demandA)), n)) // raises awaiting_input on ownID@2
			h := g.holds(n)
			if len(h) != 1 || h[0].Hold != "awaiting_input" {
				t.Fatalf("the holds: %+v", h)
			}
			refusedOnReplay(t, g, checked, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return cleared(tx, n, h[0].ID, cause)
			}, "the wait on "+ownID+"@2")
		})
	}
}

// A removal (13:206, M1 P6): once its question left the lineage, a
// supersession removes an answer input, and retires its wait as
// question_moved; the M1 fold admits it, and the log rebuilds. M0's fold
// refuses one (TestSupersedeHoldsRefused).
func TestSupersedeRemovesAnswer(t *testing.T) {
	g, _ := openFolds(t)
	review, _, next := g.superseding()
	second := g.heldRound(review)
	data := on(t, "awaiting_input", ownB+"@2")
	g.must(raised(second, data, nil))
	if err := g.do(moving(second, next, Moves{"data": nil})); err == nil || !strings.Contains(err.Error(), "has not moved") {
		t.Fatalf("a removal while the lineage asks the question: %v", err)
	}
	g.must(asked(ownB, review, nil)) // the question leaves the lineage
	g.must(moving(second, next, Moves{"data": nil}))
	p := g.supersession()
	if slices.ContainsFunc(p.Replacement.Inputs, func(in types.InputSpec) bool { return in.Name == "data" }) ||
		!slices.Equal(p.RetiredHolds, []types.RetiredHold{{Hold: data.ID, Reason: types.RetiredHoldReasonQuestionMoved}}) {
		t.Fatalf("the removal: %+v %+v", p.Replacement.Inputs, p.RetiredHolds)
	}
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}
