// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gate"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// stubReviewer is the stub registry's reviewer.
const stubReviewer = types.ExecutorID("exe_01m4tm99haemr8ajyf09v4ytzb")

// stubRegistry is the stub registry with its executors edited: builders
// and reviewers each the stub builder's or reviewer's entry under that id
// and family (nil: the stub's own), the account's group listing exactly
// them.
func stubRegistry(t *testing.T, builders, reviewers map[types.ExecutorID]types.Name) registry.Registry {
	t.Helper()
	const config = "../../fixtures/m1/config/"
	doc, err := registry.Load(config + "registry-stub.json")
	if err != nil {
		t.Fatal(err)
	}
	var gs []types.ResourceGroup
	raw, err := os.ReadFile(config + "groups.json")
	if err == nil {
		err = json.Unmarshal(raw, &gs)
	}
	if err != nil {
		t.Fatal(err)
	}
	tmpl := map[types.RegistryExecutorRole]types.RegistryExecutor{}
	for _, e := range doc.Executors {
		tmpl[e.Role] = e
	}
	var executors []types.RegistryExecutor
	for role, set := range map[types.RegistryExecutorRole]map[types.ExecutorID]types.Name{types.RegistryExecutorRoleBuilder: builders, types.RegistryExecutorRoleReviewer: reviewers} {
		if set == nil {
			executors = append(executors, tmpl[role])
		}
		for id, family := range set {
			e := tmpl[role]
			e.ID, e.Family = id, family
			executors = append(executors, e)
		}
	}
	slices.SortFunc(executors, func(a, b types.RegistryExecutor) int { return strings.Compare(string(a.ID), string(b.ID)) })
	doc.Executors = executors
	for i, g := range gs { // the account's group lists exactly the registered executors on it
		if slices.Contains(g.Members, stubReviewer) {
			gs[i].Members = []types.ExecutorID{}
			for _, e := range executors {
				if e.Group == g.ID {
					gs[i].Members = append(gs[i].Members, e.ID)
				}
			}
		}
	}
	reg, err := registry.New(doc, gs)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// reviewing makes rw's loop act on reviews under reg (an adcd start with
// it): the review gate open, the reviews and the placer under reg; a
// gate's run is 22a-5c-2b-3's (gateWorld).
func (rw *runWorld) reviewing(reg registry.Registry) {
	rw.x.Env.Families = reg
	rw.x.Reviews = reviews{reg: reg, env: rw.x.Env}
	pl := rw.x.Placer.(placer)
	pl.d.Registry = reg
	rw.x.Placer = pl
	rw.x.Acceptor = rw.r
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true, Reviews: true}, time.Hour, io.Discard)
}

// reviewNodes are the task's review and requirement nodes, by kind, the
// last of each kind in record order that is not superseded.
func reviewNodes(rw *runWorld) map[types.NodeKind]graph.NodeView {
	out := map[types.NodeKind]graph.NodeView{}
	rw.w.view(func(tx *eventstore.Tx) error {
		nodes, err := graph.TaskNodes(tx, rw.task)
		for _, n := range nodes {
			if (n.Node.Kind == types.NodeKindReview || n.Node.Kind == types.NodeKindReviewRequirement) && n.State.State != types.NodeStateStateSuperseded {
				out[n.Node.Kind] = n
			}
		}
		return err
	})
	return out
}

// gateCompleted is a run world whose task's gate completed green.
func gateCompleted(t *testing.T) *runWorld {
	t.Helper()
	rw := gateWorld(t, goEvents("TestA=pass"), map[string]string{"README.md": "hello\n"}, nil)
	if v := rw.gating(t); v.State.State != types.NodeStateStateCompleted {
		t.Fatalf("the gate: %s, holds %+v", v.State.State, v.State.Holds)
	}
	return rw
}

// P22a-6a: once the gate completed, the loop records the task's review
// late, with the roster the registry derives (lead/p22a6a-design.md): its
// requirement, committed by the controller (its conclusions its params),
// and the review on the plan, the patch, the gate's result and the
// requirement, rostered on the registry's independent reviewer, claimed
// (P16b's claim half), then placed on that reviewer.
func TestTheReviewIsRecordedOnceTheGateCompleted(t *testing.T) {
	rw := gateCompleted(t)
	rw.reviewing(stubRegistry(t, nil, nil))
	rw.passesFor(t, 6, 0)
	got := reviewNodes(rw)
	review, req := got[types.NodeKindReview], got[types.NodeKindReviewRequirement]
	p, err := review.Node.ParamsAsReviewNodeParams()
	if err != nil || len(p.Roster) != 1 || p.Roster[0].Executor != stubReviewer || req.State.State != types.NodeStateStateCompleted ||
		review.State.State != types.NodeStateStateClaimed || len(review.Node.Inputs) != 4 {
		t.Fatalf("the review: %+v (%v), its requirement %s", review, err, req.State.State)
	}
	if l, live := leaseOn(rw.w, review.Node.ID); !live || l.Executor != stubReviewer || l.Role != "reviewer" {
		t.Errorf("the review's lease: %v %+v", live, l)
	}
}

// P22a-6a: with review inactive, nothing of the review is recorded.
func TestNoReviewWhileReviewIsInactive(t *testing.T) {
	rw := gateCompleted(t)
	rw.reviewing(stubRegistry(t, nil, nil))
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	rw.passesFor(t, 4, 0)
	if got := reviewNodes(rw); len(got) != 0 {
		t.Errorf("review nodes while review is inactive: %+v", got)
	}
}

// P22a-6a (Main: the hold clears by itself; the Unblock pattern):
//   - a start whose registry no longer knows the build's executor (its
//     contributor's family unknown: no reviewer is independent of it) holds
//     the review reviewer_unavailable, and the hold stays while that is so;
//     a restart with the builder known again clears it, and the review is
//     claimed and placed;
//   - a start whose registry derives another family for the review's
//     reviewer (an independent one) holds it, then records the
//     requirement's next revision on that roster and supersedes the review
//     onto it, and the replacement is claimed and placed.
func TestAReviewerUnavailableHoldClearsByItself(t *testing.T) {
	t.Run("a contributor unknown, then known", func(t *testing.T) {
		rw := gateCompleted(t)
		rw.reviewing(stubRegistry(t, nil, nil))
		rw.passesFor(t, 2, 0) // the review and its requirement recorded, the requirement committed
		other := types.ExecutorID(newID(t, "exe"))
		rw.reviewing(stubRegistry(t, map[types.ExecutorID]types.Name{other: "stub-builder"}, nil)) // a restart: the build's executor unknown
		rw.passesFor(t, 6, 0)
		held := reviewNodes(rw)[types.NodeKindReview]
		if held.State.State != types.NodeStateStateHeld || len(held.State.Holds) != 1 || held.State.Holds[0].Hold != vocab.HoldReviewerUnavailable {
			t.Fatalf("the review with its contributor unknown: %s, holds %+v", held.State.State, held.State.Holds)
		}
		rw.passesFor(t, 4, 0)
		if still := reviewNodes(rw)[types.NodeKindReview]; len(still.State.Holds) != 1 || still.State.Holds[0].ID != held.State.Holds[0].ID {
			t.Fatalf("the hold did not stay while no reviewer is independent: %+v, was %+v", still.State.Holds, held.State.Holds)
		}
		rw.reviewing(stubRegistry(t, nil, nil)) // a restart with the builder known again
		rw.passesFor(t, 6, 0)
		review := reviewNodes(rw)[types.NodeKindReview]
		if review.Node.ID != held.Node.ID || review.State.State != types.NodeStateStateClaimed {
			t.Fatalf("the review after its contributor is known: %s, holds %+v", review.State.State, review.State.Holds)
		}
		if l, live := leaseOn(rw.w, review.Node.ID); !live || l.Executor != stubReviewer {
			t.Errorf("the review's lease: %v %+v", live, l)
		}
	})
	t.Run("its reviewer's family changed", func(t *testing.T) {
		rw := gateCompleted(t)
		rw.reviewing(stubRegistry(t, nil, nil))
		rw.passesFor(t, 2, 0)
		first := reviewNodes(rw)[types.NodeKindReview]
		rw.reviewing(stubRegistry(t, nil, map[types.ExecutorID]types.Name{stubReviewer: "another-reviewer"})) // a restart
		rw.passesFor(t, 10, 0)
		review := reviewNodes(rw)[types.NodeKindReview]
		p, err := review.Node.ParamsAsReviewNodeParams()
		if err != nil || review.Node.ID == first.Node.ID || len(p.Roster) != 1 || p.Roster[0].Family != "another-reviewer" || review.State.State != types.NodeStateStateClaimed {
			t.Fatalf("the review after its reviewer's family changed: %+v (%v)", review, err)
		}
		if l, live := leaseOn(rw.w, review.Node.ID); !live || l.Executor != stubReviewer {
			t.Errorf("the replacement's lease: %v %+v", live, l)
		}
	})
}

// Mythos #337 L2: with review active, Validate refuses a configuration
// with no registry, or a registry with no reviewer.
func TestReviewNeedsARegistryWithAReviewer(t *testing.T) {
	reviewActive = func() bool { return true }
	t.Cleanup(func() {
		reviewActive = func() bool { return false }
	})
	if err := (Config{StateDir: "/srv/adc/state"}).Validate(); err == nil || !strings.Contains(err.Error(), "--registry") {
		t.Errorf("review active, no registry: %v", err)
	}
}

// Mythos #337 L2, while review is inactive: adcd states at start, once,
// that reviews hold with no reviewer registered, and says nothing with one.
func TestAStartWithNoReviewerSaysReviewsHold(t *testing.T) {
	for name, c := range map[string]struct {
		reg  registry.Registry
		says bool
	}{"no registry": {registry.Registry{}, true}, "a reviewer registered": {stubRegistry(t, nil, nil), false}} {
		var log bytes.Buffer
		reconciler(&Daemon{}, c.reg, nil, nil, nil, 0, &log)
		if says := strings.Contains(log.String(), "reviews hold until a registry with a reviewer is given"); says != c.says {
			t.Errorf("%s: the start log %q", name, log.String())
		}
	}
}

// P22a-6a (P16b): the roster is the first admitted reviewer of no
// contributor's family: a start whose registry names the build's executor
// as a reviewer (its family the contributor's) records the review on
// another reviewer, never on it.
func TestTheRosterIsIndependentOfTheContributors(t *testing.T) {
	rw := gateCompleted(t)
	builder := types.ExecutorID(newID(t, "exe"))
	rw.reviewing(stubRegistry(t, map[types.ExecutorID]types.Name{builder: "another-builder"},
		map[types.ExecutorID]types.Name{stubBuilder: "stub-builder", stubReviewer: "stub-reviewer"}))
	rw.passesFor(t, 1, 0)
	review := reviewNodes(rw)[types.NodeKindReview]
	p, err := review.Node.ParamsAsReviewNodeParams()
	if err != nil || len(p.Roster) != 1 || p.Roster[0].Executor != stubReviewer {
		t.Errorf("the roster with a contributor's family among the reviewers: %+v (%v)", p.Roster, err)
	}
}

// Opus 22a-6a H1: two restarts with other families, the second right after
// the first's supersession (its replacement recorded, not yet claimed):
// each re-derivation records the next revision of the review's own
// requirement and supersedes onto it in one command, so the replacement is
// claimed on the last roster, and no pass refuses a review action.
func TestTwoRestartsSupersedeOntoTheLastRoster(t *testing.T) {
	rw := gateCompleted(t)
	rw.reviewing(stubRegistry(t, nil, nil))
	rw.passesFor(t, 2, 0)
	rw.reviewing(stubRegistry(t, nil, map[types.ExecutorID]types.Name{stubReviewer: "fam-b"}))
	rw.passesFor(t, 2, 0) // held, then superseded onto revision 2: its replacement waits for it
	var log bytes.Buffer
	rw.reviewing(stubRegistry(t, nil, map[types.ExecutorID]types.Name{stubReviewer: "fam-c"}))
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true, Reviews: true}, time.Hour, &log)
	rw.passesFor(t, 10, 0)
	review := reviewNodes(rw)[types.NodeKindReview]
	p, err := review.Node.ParamsAsReviewNodeParams()
	if err != nil || len(p.Roster) != 1 || p.Roster[0].Family != "fam-c" || review.State.State != types.NodeStateStateClaimed {
		t.Fatalf("the review after two restarts: %s, roster %+v (%v), holds %+v", review.State.State, p.Roster, err, review.State.Holds)
	}
	if strings.Contains(log.String(), "reconcile.review refused") {
		t.Errorf("a review action was refused: %s", log.String())
	}
}

// Opus 22a-6a M1: the loop commits only a requirement a review that is not
// superseded takes: a claimed requirement node no review takes (an orphan)
// is never given to Require, and a held one never to ReviewUnblock (re-check
// H1).
func TestAnOrphanRequirementIsNotCommitted(t *testing.T) {
	rw := gateCompleted(t)
	reg := stubRegistry(t, nil, nil)
	rw.reviewing(reg)
	rw.passesFor(t, 2, 0)
	roster, err := graph.Roster([]types.ExecutorID{stubReviewer}, reg)
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := reviews{reg: reg}.requirement(rw.task, graphProject, roster, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := rw.w.try(rw.w.sys, func(*eventstore.Tx) ([]eventstore.Event, error) { return graph.RecordReview(orphan, reg) }); err != nil {
		t.Fatal(err)
	}
	if err := rw.w.try(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.ClaimReview(tx, orphan.Node.ID, map[string]graph.Resolved{}, 1, reg)
	}); err != nil {
		t.Fatal(err)
	}
	held, err := reviews{reg: reg}.requirement(rw.task, graphProject, roster, 8)
	if err != nil {
		t.Fatal(err)
	}
	hid := types.HoldID(newID(t, "hld"))
	for _, step := range []func(*eventstore.Tx) ([]eventstore.Event, error){
		func(*eventstore.Tx) ([]eventstore.Event, error) { return graph.RecordReview(held, reg) },
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Raise(tx, held.Node.ID, types.NodeHold{ID: hid, Hold: vocab.HoldReviewerUnavailable, SignalInstance: "registry:" + string(stubReviewer),
				Owner: types.OwnerSystem, Pages: []types.NodeHoldPagesItem{}}, nil)
		},
	} {
		if err := rw.w.try(rw.w.sys, step); err != nil {
			t.Fatal(err)
		}
	}
	var s reconcile.Snapshot
	rw.w.view(func(tx *eventstore.Tx) (err error) {
		s, err = reconcile.Observe(tx, reconcile.Gates{Admits: true, Evaluates: true, Reviews: true})
		return err
	})
	for _, v := range s.Requirements {
		if v.ID == string(orphan.Node.ID) {
			t.Errorf("the orphan requirement is given to Require: %+v", s.Requirements)
		}
	}
	for _, h := range s.ReviewHeld {
		if h.Node.ID == string(held.Node.ID) {
			t.Errorf("the held orphan requirement is given to ReviewUnblock: %+v", s.ReviewHeld)
		}
	}
}

// the review state table's registries (Opus 22a-6a re-check: the state
// table): A the stub's; N its build's executor unknown (no reviewer
// independent: the review's claim refused); NB that, and the reviewer of
// another family (the requirement's claim refused, none independent); NX
// that with a third family; B and C the reviewer of another family,
// independent.
type tableRegistry int

const (
	regA tableRegistry = iota
	regN
	regNB
	regNX
	regB
	regC
)

func (k tableRegistry) of(t *testing.T, unknown types.ExecutorID) registry.Registry {
	builders := map[types.ExecutorID]types.Name{unknown: "stub-builder"}
	switch k {
	case regN:
		return stubRegistry(t, builders, nil)
	case regNB:
		return stubRegistry(t, builders, map[types.ExecutorID]types.Name{stubReviewer: "fam-b"})
	case regNX:
		return stubRegistry(t, builders, map[types.ExecutorID]types.Name{stubReviewer: "fam-x"})
	case regB:
		return stubRegistry(t, nil, map[types.ExecutorID]types.Name{stubReviewer: "fam-b"})
	case regC:
		return stubRegistry(t, nil, map[types.ExecutorID]types.Name{stubReviewer: "fam-c"})
	}
	return stubRegistry(t, nil, nil)
}

// tableStep is a start (a restart) under a registry, then passes.
type tableStep struct {
	reg    tableRegistry
	passes int
}

// cellOf is the live requirement's and the live review's states.
func cellOf(rw *runWorld) string {
	got := reviewNodes(rw)
	return string(got[types.NodeKindReviewRequirement].State.State) + "/" + string(got[types.NodeKindReview].State.State)
}

// heldChain is the reviewer_unavailable hold on the live review or the
// requirement it waits on ("": none).
func heldChain(rw *runWorld) types.HoldID {
	got := reviewNodes(rw)
	for _, k := range []types.NodeKind{types.NodeKindReview, types.NodeKindReviewRequirement} {
		for _, h := range got[k].State.Holds {
			if h.Hold == vocab.HoldReviewerUnavailable {
				return h.ID
			}
		}
	}
	return ""
}

// The review state table (Opus 22a-6a re-check H1, L1; pr-6a.md): from
// each reachable cell (the live requirement × the live review), each
// registry event (a restart to no independent roster, back to the
// original, to another independent roster) ends with the review claimed and placed
// on the registry's independent roster, or held while none exists and
// claimed on it once a restart restores it; a review claimed before the
// event stays claimed (its commit's, 22a-6c: L2); no review action is
// refused in any pass; and no requirement node is left held or open that
// no live review takes, nor a committed one retired.
func TestTheReviewStateTable(t *testing.T) {
	starts := []struct {
		name  string
		steps []tableStep
		cell  string
	}{
		{"requirement recorded, review waiting", []tableStep{{regA, 1}}, "runnable/waiting"},
		{"requirement held, review waiting", []tableStep{{regA, 1}, {regNB, 3}}, "held/waiting"},
		{"requirement claimed, review waiting", []tableStep{{regA, 2}}, "claimed/waiting"},
		{"requirement completed, review waiting", []tableStep{{regA, 3}}, "completed/waiting"},
		{"requirement completed, review held", []tableStep{{regA, 2}, {regN, 3}}, "completed/held"},
		{"requirement completed, review claimed", []tableStep{{regA, 6}}, "completed/claimed"},
		{"requirement completed, review claimed, unplaced", []tableStep{{regA, 4}}, "completed/claimed"},
		{"a replacement held, its requirement held", []tableStep{{regA, 2}, {regN, 3}, {regB, 1}, {regNX, 3}}, "held/held"},
		{"requirement held, another roster independent", []tableStep{{regA, 1}, {regB, 1}}, "held/waiting"},
		{"a replacement waiting on its requirement, moved off a held one", []tableStep{{regA, 1}, {regB, 2}}, "runnable/waiting"},
		{"requirement completed, review held, another roster independent", []tableStep{{regA, 2}, {regB, 2}}, "completed/held"},
	}
	events := []struct {
		name    string
		reg     tableRegistry
		family  types.Name // the claimed roster's; "": held, until a restart to A
		claimed bool
	}{
		{"to no independent roster", regN, "", false},
		{"restored", regA, "stub-reviewer", true},
		{"to another independent roster", regC, "fam-c", true},
	}
	for _, s := range starts {
		for _, e := range events {
			t.Run(s.name+", "+e.name, func(t *testing.T) {
				rw := gateCompleted(t)
				unknown := types.ExecutorID(newID(t, "exe"))
				for _, st := range s.steps {
					rw.reviewing(st.reg.of(t, unknown))
					rw.passesFor(t, st.passes, 0)
				}
				start := cellOf(rw)
				if s.cell != "" && start != s.cell {
					t.Fatalf("the start: %s, want %s", start, s.cell)
				}
				before := reviewNodes(rw)[types.NodeKindReview]
				_, placed := leaseOn(rw.w, before.Node.ID)
				if before.State.State == types.NodeStateStateClaimed && strings.HasSuffix(s.name, "unplaced") == placed {
					t.Fatalf("the start: placed %v", placed)
				}
				var log bytes.Buffer
				event := func(k tableRegistry) {
					rw.reviewing(k.of(t, unknown))
					rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true, Reviews: true}, time.Hour, &log)
					rw.passesFor(t, 10, 0)
				}
				event(e.reg)
				want := e.family
				if before.State.State == types.NodeStateStateClaimed && want == "" { // its roster is still the registry's: Match places it; its contributor is the commit's to refuse (L2, 22a-6c)
					want = "stub-reviewer"
				}
				if before.State.State == types.NodeStateStateClaimed && placed { // L2: the commit's (22a-6c)
					if after := reviewNodes(rw)[types.NodeKindReview]; after.Node.ID != before.Node.ID || after.State.State != types.NodeStateStateClaimed {
						t.Fatalf("the claimed review after the event: %s %s", after.Node.ID, after.State.State)
					}
					if l, live := leaseOn(rw.w, before.Node.ID); !live || l.Executor != stubReviewer {
						t.Fatalf("its lease: %v %+v", live, l)
					}
					return
				}
				if want == "" {
					held := heldChain(rw)
					if held == "" {
						t.Fatalf("no independent roster, nothing held: %s", cellOf(rw))
					}
					rw.passesFor(t, 4, 0)
					if heldChain(rw) != held {
						t.Fatalf("the hold did not stay: %s, was %s", heldChain(rw), held)
					}
					event(regA)
					want = "stub-reviewer"
				}
				review := reviewNodes(rw)[types.NodeKindReview]
				p, err := review.Node.ParamsAsReviewNodeParams()
				if err != nil || len(p.Roster) != 1 || p.Roster[0].Family != want || review.State.State != types.NodeStateStateClaimed {
					t.Fatalf("from %s: %s, roster %+v (%v), want claimed on %s", start, cellOf(rw), p.Roster, err, want)
				}
				if l, live := leaseOn(rw.w, review.Node.ID); !live || l.Executor != p.Roster[0].Executor { // placed on it (Astra chunk 3 H1)
					t.Fatalf("from %s: its lease %v %+v", start, live, l)
				}
				if log.Len() != 0 {
					t.Errorf("the loop logged: %s", log.String())
				}
				rw.w.view(func(tx *eventstore.Tx) error {
					nodes, err := graph.TaskNodes(tx, rw.task)
					for _, n := range nodes {
						switch {
						case n.Node.Kind != types.NodeKindReviewRequirement:
						case n.State.Result != nil && n.State.State != types.NodeStateStateCompleted:
							t.Errorf("committed requirement %s %s: a supersession moving off it retired it", n.Node.ID, n.State.State)
						case n.State.State != types.NodeStateStateCompleted && n.State.State != types.NodeStateStateSuperseded:
							t.Errorf("requirement %s left %s", n.Node.ID, n.State.State)
						}
					}
					return err
				})
			})
		}
	}
}

// Opus 22a-6a re-check L1: a supersession that moves a review off its
// requirement retires that requirement only when no other review that is
// not superseded takes it: one a second review takes stays open.
func TestARequirementAnotherReviewTakesIsNotRetired(t *testing.T) {
	rw := gateCompleted(t)
	reg := stubRegistry(t, nil, nil)
	rw.reviewing(reg)
	rw.passesFor(t, 1, 0)
	got := reviewNodes(rw)
	review, req := got[types.NodeKindReview], got[types.NodeKindReviewRequirement]
	second := review.Node
	second.ID = types.NodeID(newID(t, "nod"))
	if err := rw.w.try(rw.w.sys, func(*eventstore.Tx) ([]eventstore.Event, error) {
		return graph.RecordReview(types.NodeRecorded{Type: "node_recorded", Task: rw.task, Project: graphProject, Node: second}, reg)
	}); err != nil {
		t.Fatal(err)
	}
	other := stubRegistry(t, nil, map[types.ExecutorID]types.Name{stubReviewer: "fam-b"})
	roster, err := graph.Roster([]types.ExecutorID{stubReviewer}, other)
	if err != nil {
		t.Fatal(err)
	}
	next, err := reviews{reg: other}.requirement(rw.task, graphProject, roster, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := rw.w.try(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.SupersedeOnto(tx, review.Node.ID, next, other)
	}); err != nil {
		t.Fatal(err)
	}
	rw.w.view(func(tx *eventstore.Tx) error {
		v, err := graph.ViewOf(tx, req.Node.ID)
		if err == nil && v.State.State == types.NodeStateStateSuperseded {
			t.Errorf("the requirement a second review takes was retired")
		}
		return err
	})
}

// P22a-6a′ (I5, I13): the review's gate input takes the gate requirement the
// log derives (gateNeed): the gate's selected result serves while its
// measurement (gate.measured/v1) holds its tree, image and baseline. A
// gate result whose receipt is of another tree than accept's selected one,
// of another environment than its recorded image's, or of another baseline
// than its recorded one, is refused as the review's input; one with no
// recorded measurement, or measured in an image the project no longer pins,
// serves no review.
func TestTheReviewsGateRequirement(t *testing.T) {
	rw := gateCompleted(t)
	nodes := templateNodes(rw.w, rw.task)
	gateNode := nodes[types.NodeKindGate]
	serves := func(edit func(*types.GateMeasured, *types.ProjectProfile)) error {
		var err error
		rw.w.view(func(tx *eventstore.Tx) error {
			g, verr := graph.ViewOf(tx, gateNode)
			if verr != nil {
				return verr
			}
			m, ok, merr := gate.MeasurementOf(tx, *g.State.Result)
			if merr != nil || !ok {
				t.Fatalf("no measurement of the gate's result: %v", merr)
			}
			prof, _, perr := authority.ProjectProfile(tx, g.Project)
			if perr != nil {
				return perr
			}
			p := *prof
			edit(&m, &p)
			var req receipt.Requirement
			req, err = requirementWith(tx, gateNode, m, p)
			if err == nil {
				raw, _, gerr := tx.Get(graph.ResultProjection, string(*g.State.Result))
				var rr types.ResultRecorded
				if gerr == nil {
					gerr = json.Unmarshal(raw, &rr)
				}
				if gerr != nil {
					return gerr
				}
				err = receiptApplies(t, rw.w, rr, req)
			}
			return nil
		})
		return err
	}
	if err := serves(func(*types.GateMeasured, *types.ProjectProfile) {}); err != nil {
		t.Fatalf("the gate's own measurement: %v", err)
	}
	for name, edit := range map[string]func(*types.GateMeasured, *types.ProjectProfile){
		"another image": func(m *types.GateMeasured, _ *types.ProjectProfile) { m.Image.ID = strings.Repeat("c", 64) },
		"another baseline": func(m *types.GateMeasured, _ *types.ProjectProfile) {
			m.BaselineDigest = "sha256:" + types.Digest(strings.Repeat("d", 64))
		},
		"an image the project no longer pins": func(m *types.GateMeasured, _ *types.ProjectProfile) {
			m.Image.Pinned = "localhost/adc/other@sha256:" + strings.Repeat("e", 64)
		},
		"another gate command": func(_ *types.GateMeasured, p *types.ProjectProfile) {
			p.Gate.Command = append(slices.Clone(p.Gate.Command), "-v")
		},
	} {
		if err := serves(edit); err == nil {
			t.Errorf("%s: the gate's result served the review", name)
		}
	}
	for name, c := range map[string]struct { // a refusal, so the review waits (Opus 22a-6a′ M1, L4)
		edit   func(*types.GateMeasured, *types.ProjectProfile)
		reason vocab.Reason
	}{
		"an image the project no longer pins": {func(m *types.GateMeasured, _ *types.ProjectProfile) {
			m.Image.Pinned = "localhost/adc/other@sha256:" + strings.Repeat("e", 64)
		}, vocab.ReasonInstrumentChanged},
		"a baseline of another base": {func(m *types.GateMeasured, _ *types.ProjectProfile) {
			m.BaseTree = "git:" + types.GitID(strings.Repeat("f", 40))
		}, vocab.ReasonEvidenceInapplicable},
	} {
		var refused *receipt.Refusal
		if err := serves(c.edit); !errors.As(err, &refused) || refused.Reason != c.reason {
			t.Errorf("%s: %v, want a %s refusal", name, err, c.reason)
		}
	}
	t.Run("another tree", func(t *testing.T) {
		rw.w.view(func(tx *eventstore.Tx) error {
			g, _ := graph.ViewOf(tx, gateNode)
			m, _, _ := gate.MeasurementOf(tx, *g.State.Result)
			prof, _, _ := authority.ProjectProfile(tx, g.Project)
			req, err := gate.RequirementFor(prof.Gate, strings.Repeat("f", 40), m, gate.Subject{Project: g.Project, Revision: buildContractRevision})
			if err != nil {
				return err
			}
			raw, _, _ := tx.Get(graph.ResultProjection, string(*g.State.Result))
			var rr types.ResultRecorded
			_ = json.Unmarshal(raw, &rr)
			if receiptApplies(t, rw.w, rr, req) == nil {
				t.Error("a gate result of another tree served the review")
			}
			return nil
		})
	})
}

// receiptApplies is whether rr's receipt applies to req (receipt.Applies),
// the receipt read from its blob.
func receiptApplies(t *testing.T, w *seeder, rr types.ResultRecorded, req receipt.Requirement) error {
	t.Helper()
	for _, r := range rr.Result.Value {
		if r.Type != types.RefTypeReceipt {
			continue
		}
		b, err := blobOf(t, w, r.ContentDigest)
		var rcpt types.Receipt
		if err == nil {
			err = json.Unmarshal(b, &rcpt)
		}
		if err != nil {
			return err
		}
		return receipt.Applies(rcpt, req, time.Now())
	}
	return errors.New("no receipt")
}

// P22a-6a′: the measurement's fold admits one per gate result, under its
// gate node, of a gate_result that node recorded: a second for the gate's
// result, one naming the build's result, the gate's result under the build,
// and the plan's result under the gate, are refused.
func TestAMeasurementIsTheGatesOwn(t *testing.T) {
	rw := gateCompleted(t)
	nodes := templateNodes(rw.w, rw.task)
	var gateResult, buildResult, planResult types.ResultID
	rw.w.view(func(tx *eventstore.Tx) error {
		g, err := graph.ViewOf(tx, nodes[types.NodeKindGate])
		b, berr := graph.ViewOf(tx, nodes[types.NodeKindBuild])
		p, perr := graph.ViewOf(tx, nodes[types.NodeKindPlan])
		gateResult, buildResult, planResult = *g.State.Result, *b.State.Result, *p.State.Result
		return errors.Join(err, berr, perr)
	})
	img := sandbox.Image{Pinned: "localhost/adc/go@sha256:" + strings.Repeat("1", 64), ID: strings.Repeat("b", 64), OS: "linux", Arch: "arm64"}
	for name, c := range map[string]struct {
		node   types.NodeID
		result types.ResultID
	}{"a second for the gate's result": {nodes[types.NodeKindGate], gateResult}, "the build's result": {nodes[types.NodeKindBuild], buildResult},
		"the gate's result under the build": {nodes[types.NodeKindBuild], gateResult}, "the plan's result under the gate": {nodes[types.NodeKindGate], planResult}} {
		if err := rw.w.try(rw.w.sys, func(*eventstore.Tx) ([]eventstore.Event, error) {
			ev, err := gate.RecordMeasured(c.node, c.result, gate.Baseline{Base: strings.Repeat("a", 40)}, img)
			return []eventstore.Event{ev}, err
		}); err == nil {
			t.Errorf("%s: recorded", name)
		}
	}
	// Opus 22a-6a′ L3: in the command that records the gate's result, never
	// later. The fold runs on its own over a re-recording of the gate's
	// result and a measurement, the measurement's own record dropped first;
	// the command is rolled back.
	var recorded json.RawMessage
	rw.w.view(func(tx *eventstore.Tx) (err error) {
		recorded, _, err = tx.Get(graph.ResultProjection, string(gateResult))
		return err
	})
	rollback := errors.New("rolled back")
	for name, c := range map[string]struct {
		measuredIn string
		twice      bool
		subject    types.NodeID
		admitted   bool
	}{"in the result's command": {"cmd_a", false, "", true}, "in a later command": {"cmd_b", false, "", false}, "twice in the result's command": {"cmd_a", true, "", false},
		"under the build, in the result's command": {"cmd_a", false, nodes[types.NodeKindBuild], false}} {
		err := rw.w.try(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			if err := tx.Delete(gate.MeasuredProjection, string(gateResult)); err != nil {
				return nil, err
			}
			m, err := gate.RecordMeasured(nodes[types.NodeKindGate], gateResult, gate.Baseline{Base: strings.Repeat("a", 40)}, img)
			if err != nil {
				return nil, err
			}
			resultCmd, measuredCmd := "cmd_a", c.measuredIn
			r := eventstore.Event{Kind: graph.ResultRecorded, Subject: string(nodes[types.NodeKindGate]), Command: &resultCmd, Payload: recorded}
			m.Command = &measuredCmd
			if c.subject != "" {
				m.Subject = string(c.subject)
			}
			if err := (gate.MeasuredProjector{}).Project(tx, []eventstore.Event{r}); err != nil {
				return nil, err
			}
			measured := []eventstore.Event{m}
			if c.twice {
				measured = append(measured, m)
			}
			if err := (gate.MeasuredProjector{}).Project(tx, measured); err != nil {
				return nil, err
			}
			return nil, rollback
		})
		if admitted := errors.Is(err, rollback); admitted != c.admitted {
			t.Errorf("a measurement %s: admitted %v (%v)", name, admitted, err)
		}
	}
}

// Opus 22a-6a′ M1, P22a-6b (Main's decision: the wait shown with its
// reason): a gate result whose requirement cannot be derived holds the
// review awaiting_receipt, its reason in the signal instance, and the rest
// of the task evaluates (the requirement completes), with no evaluation
// refused:
//   - no recorded measurement (its record dropped here, as a result
//     recorded before 6a′ would have none): receipt_incomplete;
//   - then measured in an image the project does not pin: the hold
//     re-raised on instrument_changed, the first cleared;
//   - then its own measurement again: the hold cleared, the review claimed.
func TestARefusedGateReceiptHoldsTheReviewWithItsReason(t *testing.T) {
	rw := gateCompleted(t)
	gateNode := templateNodes(rw.w, rw.task)[types.NodeKindGate]
	var result types.ResultID
	var own types.GateMeasured
	rw.w.view(func(tx *eventstore.Tx) error {
		g, err := graph.ViewOf(tx, gateNode)
		if err == nil {
			result = *g.State.Result
			own, _, err = gate.MeasurementOf(tx, result)
		}
		return err
	})
	measure := func(m *types.GateMeasured) {
		t.Helper()
		if err := rw.w.try(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			if m == nil {
				return nil, tx.Delete(gate.MeasuredProjection, string(result))
			}
			raw, err := json.Marshal(*m)
			if err != nil {
				return nil, err
			}
			return nil, tx.Put(gate.MeasuredProjection, string(result), raw)
		}); err != nil {
			t.Fatal(err)
		}
	}
	held := func(signal string) types.HoldID {
		t.Helper()
		review := reviewNodes(rw)[types.NodeKindReview]
		if review.State.State != types.NodeStateStateHeld || len(review.State.Holds) != 1 || review.State.Holds[0].Hold != vocab.HoldAwaitingReceipt ||
			review.State.Holds[0].SignalInstance != signal || review.State.Holds[0].Reason != nil {
			t.Fatalf("the review: %s, holds %+v, want held awaiting_receipt on %s", review.State.State, review.State.Holds, signal)
		}
		return review.State.Holds[0].ID
	}
	measure(nil)
	var log bytes.Buffer
	rw.reviewing(stubRegistry(t, nil, nil))
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true, Reviews: true}, time.Hour, &log)
	rw.passesFor(t, 6, 0)
	if req := reviewNodes(rw)[types.NodeKindReviewRequirement]; req.State.State != types.NodeStateStateCompleted {
		t.Fatalf("the requirement: %s", req.State.State)
	}
	first := held("receipt:gate:receipt_incomplete")
	rw.passesFor(t, 3, 0)
	if again := held("receipt:gate:receipt_incomplete"); again != first {
		t.Fatalf("the hold did not stay: %s, was %s", again, first)
	}
	unpinned := own
	unpinned.Image.Pinned = "localhost/adc/other@sha256:" + strings.Repeat("e", 64)
	measure(&unpinned)
	rw.passesFor(t, 3, 0)
	if next := held("receipt:gate:instrument_changed"); next == first {
		t.Fatalf("the changed reason kept the hold %s", first)
	}
	measure(&own)
	rw.passesFor(t, 1, 0) // the hold cleared and the review claimed in one evaluation
	if review := reviewNodes(rw)[types.NodeKindReview]; review.State.State != types.NodeStateStateClaimed || len(review.State.Holds) != 0 {
		t.Fatalf("the review once its receipt serves: %s, holds %+v", review.State.State, review.State.Holds)
	}
	rw.passesFor(t, 4, 0)
	if log.Len() != 0 {
		t.Errorf("the loop logged: %s", log.String())
	}
}

// Opus 22a-6b M1: the evaluator clears only the awaiting_receipt holds it
// raised (graph.ReceiptWait): another's, on evidence:rcp_x (as graph.Retry
// raises one), stays held while the review's inputs would serve.
func TestAnotherReceiptWaitIsNotTheEvaluators(t *testing.T) {
	rw := gateCompleted(t)
	rw.reviewing(stubRegistry(t, nil, nil))
	rw.passesFor(t, 1, 0)
	review := reviewNodes(rw)[types.NodeKindReview].Node.ID
	if err := rw.w.try(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Raise(tx, review, types.NodeHold{ID: types.HoldID(newID(t, "hld")), Hold: vocab.HoldAwaitingReceipt, SignalInstance: "evidence:rcp_x",
			Owner: types.OwnerSystem, Pages: []types.NodeHoldPagesItem{}}, nil)
	}); err != nil {
		t.Fatal(err)
	}
	rw.passesFor(t, 6, 0)
	v := reviewNodes(rw)[types.NodeKindReview]
	if v.State.State != types.NodeStateStateHeld || len(v.State.Holds) != 1 || v.State.Holds[0].SignalInstance != "evidence:rcp_x" {
		t.Fatalf("the review: %s, holds %+v", v.State.State, v.State.Holds)
	}
}
