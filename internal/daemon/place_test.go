// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/dispatch"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// stubBuilder is registry-stub.json's builder: $4 per million tokens at its
// higher rate, a 500-token turn, 2 turns.
const stubBuilder = types.ExecutorID("exe_01m4tm99haemqt8es2qfbwj8pa")

// placing is a seeder over adcd's projector with the fixture groups, its
// system principal, the stub registry, and an executor whose placer places
// on it under rails caps (*perDay the day's USD rail, in micros).
func placing(t *testing.T, perDay *int64) (*seeder, types.ActorID, reconcile.Executor, placer) {
	t.Helper()
	return placingWith(t, perDay, nil)
}

// placingWith is placing, its project's profile edited by edit (nil: none)
// and its rails nil when perDay is.
func placingWith(t *testing.T, perDay *int64, edit func(map[string]any)) (*seeder, types.ActorID, reconcile.Executor, placer) {
	t.Helper()
	const config = "../../fixtures/m1/config/"
	var gs []types.ResourceGroup
	raw, err := os.ReadFile(config + "groups.json")
	if err == nil {
		err = json.Unmarshal(raw, &gs)
	}
	doc, lerr := registry.Load(config + "registry-stub.json")
	if err != nil || lerr != nil {
		t.Fatal(err, lerr)
	}
	reg, err := registry.New(doc, gs)
	if err != nil {
		t.Fatal(err)
	}
	w := seed(t, stateDir(t), daemonProjection(t, gs...))
	sys := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	sys.CreatedBy = sys.ID
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sys) })
	w.sys = sys.ID
	claim, err := w.s.ClaimSystem()
	if err != nil {
		t.Fatal(err)
	}
	var p identity.Principal
	w.view(func(tx *eventstore.Tx) (err error) {
		p, err = identity.SystemPrincipal(tx, claim, sys.ID)
		return err
	})
	rails := func(tx *eventstore.Tx, task types.TaskID) (graph.Caps, error) {
		c := graph.DefaultCaps
		c.PerTaskMicros, c.PerDayMicros = 1<<50, 1<<50
		if perDay != nil {
			c.PerDayMicros = *perDay
		}
		return c, nil
	}
	pl := placer{d: dispatch.Dispatcher{Registry: reg, Rails: rails}}
	if perDay == nil {
		pl.d.Rails = nil
	}
	x := reconcile.Executor{Store: w.s, System: p, Kinds: nodeKinds(), Placer: pl,
		Env: graph.Env{Authority: authority.Answers{}, Need: needs, Blobs: w.blobs, Families: reg, Rails: rails}}
	x.Planner = planner{blobs: w.blobs, env: x.Env} // the loop records the plan's brief itself (22a-5c-3)
	// The project's profile allows the stub destination.
	op := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "operator", CreatedBy: sys.ID}
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(op) })
	w.op = op.ID
	prj := string(graphProject)
	w.do(op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
		raw, err := os.ReadFile("../../fixtures/m1/profiles/py-mini.json")
		var prof map[string]any
		if err == nil {
			err = json.Unmarshal(raw, &prof)
		}
		if err != nil {
			return nil, err
		}
		prof["id"] = prj
		prof["data_policy"].(map[string]any)["allowed_destinations"] = []string{"stub"}
		if edit != nil {
			edit(prof)
		}
		payload, err := json.Marshal(prof)
		return []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.ProjectCreated, Subject: prj, Project: &prj, Payload: payload}}, err
	})
	return w, sys.ID, x, pl
}

// passes runs passes until one applies nothing (at most 10).
func passes(t *testing.T, l *reconcile.Loop) {
	t.Helper()
	for range 10 {
		ran, err := l.Pass(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if ran == 0 {
			return
		}
	}
	t.Fatal("the loop never settles")
}

// leaseOn is the live lease on node's unit, if any.
func leaseOn(w *seeder, node types.NodeID) (types.Lease, bool) {
	var l types.Lease
	var live bool
	w.view(func(tx *eventstore.Tx) error {
		id, ok, err := lease.LiveOn(tx, graph.UnitOf(node))
		if err == nil && ok {
			l, _, live, err = lease.LeaseOf(tx, id)
		}
		return err
	})
	return l, live
}

// P22a-5c-1 (I7, I10, I16, #226 entries 103 and 120), through the loop and
// adcd's placer: once the plan's result is in, the build is claimed and
// matched on the stub builder, the reservation its prior and the rails give
// (a full attempt_cap, 1,000 tokens, under a wide day rail), with the lease's
// footprint the prior. Under a day rail that leaves less than a turn, the
// match is refused and the node held budget_rail (rail:per_day_usd); the
// loop does not clear the hold while the day leaves no turn, and clears it,
// then places the build, once a profile change (here, the rails) leaves
// one: 750 tokens with 3,000 micros left.
func TestTheLoopPlacesTheBuild(t *testing.T) {
	for name, c := range map[string]struct {
		perDay int64
		tokens int64
	}{
		"a wide day rail":    {1 << 50, 1000},
		"less than one turn": {1999, 750},
	} {
		t.Run(name, func(t *testing.T) {
			perDay := c.perDay
			w, sys, x, _ := placing(t, &perDay)
			_, task := readyTask(t, w, sys)
			l := reconcile.NewLoop(x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
			passes(t, l)
			build := templateNodes(w, task)[types.NodeKindBuild]
			passes(t, l)
			if c.perDay < 1<<50 {
				st := nodesOf(w, task)[types.NodeKindBuild].State
				if st.State != types.NodeStateStateHeld || len(st.Holds) != 1 || st.Holds[0].Hold != vocab.HoldBudgetRail ||
					st.Holds[0].SignalInstance != graph.RailSignal(graph.RailDayUSD) {
					t.Fatalf("the build under a day rail leaving no turn: %+v", st)
				}
				if _, live := leaseOn(w, build); live {
					t.Fatal("a lease was placed past the day rail")
				}
				passes(t, l) // the day still leaves no turn: the hold stays
				if st := nodesOf(w, task)[types.NodeKindBuild].State; st.State != types.NodeStateStateHeld {
					t.Fatalf("the hold was cleared while the day leaves no turn: %+v", st)
				}
				perDay = 3000
				passes(t, l)
			}
			ls, live := leaseOn(w, build)
			if !live || ls.Executor != stubBuilder || ls.Reservations[0].Amount != c.tokens || ls.Footprint.ContextTokens != 500 || ls.Footprint.ExpectedTurns != 2 {
				t.Fatalf("the build's lease: %v %+v", live, ls)
			}
		})
	}
}

// fakeImages finds every image, or none.
type fakeImages struct{ present bool }

func (f fakeImages) Resolve(context.Context, string) (sandbox.Image, error) {
	if !f.present {
		return sandbox.Image{}, sandbox.ErrImage
	}
	return sandbox.Image{}, nil
}

// fakeCredentials holds every credential.
type fakeCredentials struct{}

func (fakeCredentials) Holds(context.Context, types.Slug, types.Slug) (bool, error) { return true, nil }

// P22a-5c-1 (I16, I21): the launch the attempt run calls, once its
// exchange is prepared: the readiness observed outside the writer, then
// dispatch.Launch in one system command. With the image present and the
// credential held, it starts the attempt at token 1 on the build's lease;
// with the image missing, it misses, starts nothing, releases the lease
// and holds the node wrong_owner.
func TestTheLaunchStartsTheMatchedBuild(t *testing.T) {
	for name, present := range map[string]bool{"ready": true, "the image missing": false} {
		t.Run(name, func(t *testing.T) {
			perDay := int64(1 << 50)
			w, sys, x, pl := placing(t, &perDay)
			pl.d.Images, pl.d.Credentials = fakeImages{present}, fakeCredentials{}
			x.Placer = pl
			_, task := readyTask(t, w, sys)
			l := reconcile.NewLoop(x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
			passes(t, l)
			build := templateNodes(w, task)[types.NodeKindBuild]
			passes(t, l)
			ls, live := leaseOn(w, build)
			if !live {
				t.Fatal("the build was not placed")
			}
			attempt := types.AttemptID(newID(t, "att"))
			miss, err := pl.launch(context.Background(), w.s, x.System, build, ls.ID, attempt, emptyBase)
			if err != nil {
				t.Fatal(err)
			}
			var a types.Attempt
			var started bool
			w.view(func(tx *eventstore.Tx) (err error) {
				a, _, started, err = lease.AttemptOf(tx, attempt)
				return err
			})
			if present {
				if miss != nil || !started || a.Status != types.AttemptStatusRunning || a.FencingToken != 1 || a.Lease != ls.ID {
					t.Fatalf("the launch: miss %v, attempt %+v", miss, a)
				}
				return
			}
			st := nodesOf(w, task)[types.NodeKindBuild].State
			if miss == nil || started || len(st.Holds) != 1 || st.Holds[0].Hold != "wrong_owner" {
				t.Fatalf("a launch with the image missing: miss %v, started %v, node %+v", miss, started, st)
			}
			if _, live := leaseOn(w, build); live {
				t.Error("the refused launch left its lease live")
			}
		})
	}
}

// Opus 22a-5c-1 M1: a build no match can place until the configuration
// changes is held source_invalid_config, the operator's, naming what to
// change, and waits held: the loop decides no Match on it again, and logs
// nothing. The profile's data policy admitting no builder, its pinning
// two images, and no USD rail for the priced builder each hold it on the
// profile.
func TestAConfigurationRefusalHoldsTheBuild(t *testing.T) {
	wide := int64(1 << 50)
	for name, c := range map[string]struct {
		perDay *int64
		edit   func(map[string]any)
	}{
		"a data policy admitting no builder": {&wide, func(p map[string]any) { p["data_policy"].(map[string]any)["allowed_destinations"] = []string{"other"} }},
		"two pinned images": {&wide, func(p map[string]any) {
			p["toolchain"].(map[string]any)["images"].(map[string]any)["go"] = "localhost/adc/go@sha256:" + strings.Repeat("1", 64)
		}},
		"no USD rail": {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			w, sys, x, _ := placingWith(t, c.perDay, c.edit)
			_, task := readyTask(t, w, sys)
			var log bytes.Buffer
			l := reconcile.NewLoop(x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, &log)
			passes(t, l)
			build := templateNodes(w, task)[types.NodeKindBuild]
			passes(t, l)
			st := nodesOf(w, task)[types.NodeKindBuild].State
			if st.State != types.NodeStateStateHeld || len(st.Holds) != 1 || st.Holds[0].Hold != "source_invalid_config" ||
				st.Holds[0].SignalInstance != dispatch.PlacementSignal("profile "+string(graphProject)) || st.Holds[0].Owner != types.OwnerOperator {
				t.Fatalf("the build: %+v", st)
			}
			if _, live := leaseOn(w, build); live {
				t.Fatal("a lease was placed")
			}
			head := w.head()
			if ran, err := l.Pass(context.Background()); err != nil || ran != 0 || w.head() != head || log.Len() != 0 {
				t.Errorf("a pass over the held build: %d, %v, head %d → %d, logged %q", ran, err, head, w.head(), log.String())
			}
		})
	}
}

// refusing is a Placer that refuses each node with its own error.
type refusing map[types.NodeID]string

func (r refusing) Place(_ *eventstore.Tx, node types.NodeID) ([]eventstore.Event, error) {
	return nil, errors.New(r[node])
}

func (refusing) Reopened(*eventstore.Tx, types.NodeID, types.HoldID) ([]eventstore.Event, error) {
	return nil, nil
}

func (refusing) Unblocked(*eventstore.Tx, types.NodeID, types.HoldID, string) ([]eventstore.Event, error) {
	return nil, nil
}

// Opus 22a-5c-1 M1: refusals are logged once per action and subject. Two
// builds refused for different reasons, pass after pass, log one line
// each, not one each pass; a refusal of a subject that changes is logged
// again.
func TestTheLoopLogsEachRefusalOnce(t *testing.T) {
	wide := int64(1 << 50)
	w, sys, x, _ := placing(t, &wide)
	_, task := readyTask(t, w, sys)
	reasons := refusing{}
	x.Placer = reasons // nothing is placed
	l := reconcile.NewLoop(x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	passes(t, l)
	build := templateNodes(w, task)[types.NodeKindBuild]
	other := types.Node{ID: types.NodeID(newID(t, "nod")), Kind: types.NodeKindBuild, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`),
		Inputs: []types.InputSpec{}, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}
	w.do(sys, func(*eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Record(types.NodeRecorded{Task: task, Project: graphProject, Node: other})
	})
	reasons[build], reasons[other.ID] = "the first reason", "the second reason"
	var log bytes.Buffer
	l = reconcile.NewLoop(x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, &log)
	for range 4 {
		if _, err := l.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := log.String(); strings.Count(got, "the first reason") != 1 || strings.Count(got, "the second reason") != 1 {
		t.Fatalf("four passes logged %q", got)
	}
	reasons[build] = "a third reason"
	if _, err := l.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := log.String(); strings.Count(got, "a third reason") != 1 || strings.Count(got, "the second reason") != 1 {
		t.Errorf("a changed refusal: %q", got)
	}
}

// Mythos 22a-5c-1 M1: a placement hold clears once the configuration
// admits the build again. A profile with no USD rail holds the build
// source_invalid_config; the rail is added (the placer's rails) and adcd
// restarted (a new loop): the loop clears the hold and matches the build.
// A source_invalid_config hold another component raised (no placement
// signal) is never cleared by the loop.
func TestAPlacementHoldClearsOnceConfigured(t *testing.T) {
	w, sys, x, pl := placingWith(t, nil, nil)
	_, task := readyTask(t, w, sys)
	l := reconcile.NewLoop(x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	passes(t, l)
	build := templateNodes(w, task)[types.NodeKindBuild]
	passes(t, l)
	if st := nodesOf(w, task)[types.NodeKindBuild].State; st.State != types.NodeStateStateHeld {
		t.Fatalf("the build under no USD rail: %+v", st)
	}
	pl.d.Rails = func(*eventstore.Tx, types.TaskID) (graph.Caps, error) { // the rails a fixed profile gives
		c := graph.DefaultCaps
		c.PerTaskMicros, c.PerDayMicros = 1<<50, 1<<50
		return c, nil
	}
	x.Placer = pl
	l = reconcile.NewLoop(x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard) // a restart
	passes(t, l)
	if st := nodesOf(w, task)[types.NodeKindBuild].State; st.State != types.NodeStateStateClaimed || len(st.Holds) != 0 {
		t.Fatalf("the build once the rail is added: %+v", st)
	}
	if _, live := leaseOn(w, build); !live {
		t.Fatal("the unblocked build was not matched")
	}
	// Another's source_invalid_config hold stays.
	other := types.NodeHold{ID: types.HoldID(newID(t, "hld")), Hold: "source_invalid_config", SignalInstance: "destination stub", Owner: types.OwnerOperator,
		Pages: []types.NodeHoldPagesItem{}}
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Raise(tx, build, other, nil) })
	passes(t, l)
	if st := nodesOf(w, task)[types.NodeKindBuild].State; len(st.Holds) != 1 || st.Holds[0].ID != other.ID {
		t.Errorf("another's hold: %+v", st)
	}
}

// Mythos 22a-5c-1 L1: holds are collected for the kinds Match places only.
// A plan held on the day's USD rail is not reopened by the loop (nor
// logged every pass): only its own owner clears it.
func TestADayHoldOnAPlanIsNotReopened(t *testing.T) {
	wide := int64(1 << 50)
	w, sys, x, _ := placing(t, &wide)
	x.Planner = nil // the plan stays claimed
	_, task := readyTask(t, w, sys)
	var log bytes.Buffer
	l := reconcile.NewLoop(x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, &log)
	passes(t, l)
	plan := templateNodes(w, task)[types.NodeKindPlan]
	hold, err := graph.RailHold(graph.RailDayUSD)
	if err != nil {
		t.Fatal(err)
	}
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Raise(tx, plan, hold, nil) })
	passes(t, l)
	if st := nodesOf(w, task)[types.NodeKindPlan].State; len(st.Holds) != 1 || st.Holds[0].ID != hold.ID || log.Len() != 0 {
		t.Errorf("the plan's day hold: %+v, logged %q", st, log.String())
	}
}
