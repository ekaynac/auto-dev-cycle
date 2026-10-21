// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/brief"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/intent"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// builderIntent is the build node's manifest.intent as intent.Builder
// makes it from the node's rendered context and task's records, for the
// stub builder at the stub destination.
func builderIntent(t *testing.T, w *seeder, env graph.Env, task types.TaskID, build types.NodeID) (types.ManifestIntent, graph.Rendered, error) {
	t.Helper()
	call := types.ManifestIntent{ID: types.ManifestID(newID(t, "cmf")), Call: types.CallID(newID(t, "cal")),
		Consumer: types.ManifestIntentConsumer{Role: "builder", Executor: stubBuilder, ModelSnapshot: "stub-builder-1", HarnessVersion: "1.0.0", CapabilityProfile: "large"},
		Policy:   types.ManifestIntentPolicy{UsageProfile: types.ManifestIntentPolicyUsageProfileStandard, DataPolicyDigest: "sha256:" + string(bytes.Repeat([]byte("c"), 64)), RendererVersions: map[string]string{}},
		Budget:   types.ManifestIntentBudget{Tokens: 1000, Bytes: 40000}, RenderedDigest: "sha256:" + string(bytes.Repeat([]byte("d"), 64)),
		Destination: "stub", Routing: types.ManifestIntentRoutingVerified}
	var mi types.ManifestIntent
	var rendered graph.Rendered
	var err error
	w.view(func(tx *eventstore.Tx) error {
		recs, lerr := intent.Load(tx, w.blobs, task)
		var cerr error
		rendered, cerr = graph.Context(tx, build, env)
		if err = errors.Join(lerr, cerr); err != nil {
			return nil
		}
		sizes := map[types.Digest]int64{}
		for _, r := range rendered.Included {
			sizes[r.IdentityDigest] = 100
		}
		mi, err = intent.Builder(intent.In{Call: call, Context: rendered, Bytes: sizes, Records: recs})
		return nil
	})
	return mi, rendered, err
}

// P22a-5c-3 (#226 entries 103 and 117): the loop records the claimed plan's
// result itself, one ref, the plan brief: a root blob in the project's
// scope under its data policy, holding the spec's title and requirement
// and its digest, never the acceptance. The build, claimed on it, takes it
// in its rendered context, and the real I18 walk (intent.Builder over
// intent.Load's records) passes it.
func TestThePlanBrief(t *testing.T) {
	perDay := int64(1 << 50)
	w, sys, x, _ := placing(t, &perDay)
	_, task := readyTask(t, w, sys)
	l := reconcile.NewLoop(x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	passes(t, l)
	nodes := nodesOf(w, task)
	plan, build := nodes[types.NodeKindPlan], nodes[types.NodeKindBuild]
	if plan.State.State != types.NodeStateStateCompleted || build.State.State != types.NodeStateStateClaimed {
		t.Fatalf("the plan %s, the build %s", plan.State.State, build.State.State)
	}
	var rec types.Task
	var r types.ResultRecorded
	var policy types.Restriction
	w.view(func(tx *eventstore.Tx) (err error) {
		if rec, err = authority.TaskOf(tx, task); err != nil {
			return err
		}
		raw, _, err := authority.ProjectDataPolicy(tx, graphProject)
		if err == nil {
			policy, err = brief.Restriction(graphProject, raw)
		}
		if err != nil {
			return err
		}
		if plan.State.Result == nil {
			t.Fatal("the plan has no selected result")
		}
		raw, _, err = tx.Get(graph.ResultProjection, string(*plan.State.Result))
		if err == nil {
			err = json.Unmarshal(raw, &r)
		}
		return err
	})
	if len(r.Result.Value) != 1 || r.Result.Kind != types.ResultKindPlan || r.Result.Reason != vocab.ReasonCompletedClean {
		t.Fatalf("the plan's result: %+v", r.Result)
	}
	ref := r.Result.Value[0]
	b, _ := blobOf(t, w, ref.ContentDigest)
	if ref.Type != types.RefTypeBlob || ref.ID != string(ref.ContentDigest) || len(ref.Lineage) != 0 || len(ref.Restrictions) != 1 ||
		ref.Restrictions[0].Project != graphProject || ref.ContentDigest != ownDigest(t, rec, policy.PolicyDigest) || !graph.SameRestriction(ref.Restrictions[0], policy) || bytes.Contains(b, []byte("accept_test.py")) {
		t.Errorf("the plan brief's ref %+v, bytes %s", ref, b)
	}
	mi, rendered, err := builderIntent(t, w, x.Env, task, build.Node.ID)
	if err != nil || len(rendered.Included) != 1 || rendered.Included[0].IdentityDigest != ref.IdentityDigest || len(mi.Included) != 1 {
		t.Fatalf("the builder's intent over the brief: %v, context %+v", err, rendered)
	}
	// Mythos 22a-5c-3 L1: the build's lease names the policy by the brief's
	// digest of it.
	if ls, live := leaseOn(w, build.Node.ID); !live || ls.PlacementChecks.DataFloor.Detail != "policy "+string(policy.PolicyDigest) {
		t.Errorf("the lease's data_floor check %q, want the brief's policy digest %s", ls.PlacementChecks.DataFloor.Detail, policy.PolicyDigest)
	}
	// Mythos 22a-5c-3 M1: an intent naming the honest brief under fewer
	// restrictions, or in another scope, is not the input its record names.
	for name, edit := range map[string]func(*types.Ref){
		"no restriction": func(r *types.Ref) { r.Restrictions = []types.Restriction{} },
		"another project": func(r *types.Ref) {
			other := types.ProjectID("prj_01m3250vkwffc979nzjw0g2sa2")
			r.Scope.Project = &other
		},
		"the framework's scope": func(r *types.Ref) { r.Scope.Framework = true },
	} {
		cp := ref
		cp.Restrictions = slices.Clone(ref.Restrictions)
		edit(&cp)
		sealed, err := knowledge.Seal(cp)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		w.view(func(tx *eventstore.Tx) error {
			recs, err := intent.Load(tx, w.blobs, task)
			if err != nil {
				return err
			}
			mi := types.ManifestIntent{Consumer: types.ManifestIntentConsumer{Role: "builder"}, Inputs: []types.Ref{sealed}}
			if err := intent.Check(recs, "builder", mi); !errors.Is(err, intent.ErrAncestryUnresolved) {
				t.Errorf("an intent naming the brief with %s: %v", name, err)
			}
			return nil
		})
	}
}

// blobOf is blob d's bytes in w's store.
func blobOf(t *testing.T, w *seeder, d types.Digest) ([]byte, error) {
	t.Helper()
	rc, err := w.blobs.Open(string(d))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// ownDigest is the digest of task t's own brief under the policy of
// digest policy.
func ownDigest(t *testing.T, task types.Task, policy types.Digest) types.Digest {
	t.Helper()
	_, d, err := brief.Of(task, policy)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// forgery is a plan ref a test records in place of the loop's brief:
// bytes put in the blob store, and the ref over them.
type forgery func(t *testing.T, task types.Task, policy types.Restriction) ([]byte, types.Ref)

// briefRef is the ref of bytes b: a root blob, its id its digest, under
// restrictions rs.
func briefRef(t *testing.T, b []byte, rs ...types.Restriction) types.Ref {
	t.Helper()
	d := fmt.Sprintf("sha256:%x", sha256.Sum256(b)) // the blob store's digest: of the bytes as they are
	return types.Ref{Type: types.RefTypeBlob, ID: d, Scope: types.RefScope{Project: new(graphProject)}, Lineage: []string{},
		Restrictions: append([]types.Restriction{}, rs...), ContentDigest: types.Digest(d)}
}

// ofTask is the brief of task edited by edit, under the project's policy.
func ofTask(edit func(*types.Task)) forgery {
	return func(t *testing.T, task types.Task, policy types.Restriction) ([]byte, types.Ref) {
		edit(&task)
		b, _, err := brief.Of(task, policy.PolicyDigest)
		if err != nil {
			t.Fatal(err)
		}
		return b, briefRef(t, b, policy)
	}
}

// #226 entry 117; Opus 22a-5c-3 H1, M1, M2: a plan node's ref is honoured
// only as its task's own brief, under its project's data policy. Each
// forgery below, recorded as the plan's result, is refused by the I18 walk
// of the build's intent (intent.ErrNotTheBrief): another task's or another
// spec's brief; a brief whose requirement carries an acceptance, with the
// right task and spec; the own brief's bytes with one leading space, under
// their own digest; the own brief under another id; as a view; with a
// lineage; with no restriction; the brief of the task's spec under another
// data policy's digest (r2 M1); the own brief in another project's scope
// (r2 L2), or in the framework's scope too (Mythos L2). A forged plan ref met as an ancestor (the
// lineage of a build's patch, the accept's input) is refused too.
func TestAForgedPlanBriefIsRefused(t *testing.T) {
	own := func(t *testing.T, task types.Task, policy types.Restriction) []byte {
		b, _, err := brief.Of(task, policy.PolicyDigest)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for name, forge := range map[string]forgery{
		"another spec digest": ofTask(func(x *types.Task) { x.SpecDigest = "sha256:" + types.Digest(bytes.Repeat([]byte("f"), 64)) }),
		"another task":        ofTask(func(x *types.Task) { x.ID = "tsk_01m3250v9xft08ssr4ygq6g2nq" }),
		"an acceptance in the requirement": ofTask(func(x *types.Task) {
			x.Spec.Requirement = "CANARY-ACCEPTANCE: func TestHidden(t *testing.T) { t.Fatal() }"
		}),
		"the own brief with one leading space": func(t *testing.T, task types.Task, policy types.Restriction) ([]byte, types.Ref) {
			b := append([]byte(" "), own(t, task, policy)...)
			return b, briefRef(t, b, policy)
		},
		"the own brief under another id": func(t *testing.T, task types.Task, policy types.Restriction) ([]byte, types.Ref) {
			b := own(t, task, policy)
			r := briefRef(t, b, policy)
			r.ID = "brief-1"
			return b, r
		},
		"the own brief as a view": func(t *testing.T, task types.Task, policy types.Restriction) ([]byte, types.Ref) {
			b := own(t, task, policy)
			r := briefRef(t, b, policy)
			r.Type = types.RefTypeView
			return b, r
		},
		"the own brief with a lineage": func(t *testing.T, task types.Task, policy types.Restriction) ([]byte, types.Ref) {
			b := own(t, task, policy)
			r := briefRef(t, b, policy)
			r.Lineage = []string{r.ID}
			return b, r
		},
		"the own brief with no restriction": func(t *testing.T, task types.Task, policy types.Restriction) ([]byte, types.Ref) {
			b := own(t, task, policy)
			return b, briefRef(t, b)
		},
		"the brief under another policy's digest": func(t *testing.T, task types.Task, policy types.Restriction) ([]byte, types.Ref) {
			b, _, err := brief.Of(task, "sha256:"+types.Digest(bytes.Repeat([]byte("e"), 64)))
			if err != nil {
				t.Fatal(err)
			}
			return b, briefRef(t, b, policy)
		},
		"the own brief in the framework's scope too": func(t *testing.T, task types.Task, policy types.Restriction) ([]byte, types.Ref) {
			b := own(t, task, policy)
			r := briefRef(t, b, policy)
			r.Scope.Framework = true
			return b, r
		},
		"the own brief in another project's scope": func(t *testing.T, task types.Task, policy types.Restriction) ([]byte, types.Ref) {
			b := own(t, task, policy)
			r := briefRef(t, b, policy)
			other := types.ProjectID("prj_01m3250vkwffc979nzjw0g2sa2")
			r.Scope.Project = &other
			return b, r
		},
	} {
		t.Run(name, func(t *testing.T) {
			w, x, task, build := forgedPlan(t, forge)
			if _, _, err := builderIntent(t, w, x.Env, task, build); !errors.Is(err, intent.ErrNotTheBrief) {
				t.Errorf("the builder's intent over the forgery: %v", err)
			}
		})
	}
	t.Run("a forgery met as an ancestor", func(t *testing.T) {
		w, x, task, build := forgedPlan(t, ofTask(func(x *types.Task) { x.Spec.Requirement = "CANARY-ACCEPTANCE" }))
		var forged types.Ref
		w.view(func(tx *eventstore.Tx) error {
			ctx, err := graph.Context(tx, build, x.Env)
			if err == nil && len(ctx.Included) == 1 {
				forged = ctx.Included[0]
			}
			return err
		})
		patch := briefRef(t, []byte("a patch"))
		patch.Lineage = []string{forged.ID}
		sealed, err := knowledge.Seal(patch)
		if err != nil {
			t.Fatal(err)
		}
		w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			v, err := graph.ViewOf(tx, build)
			if err != nil {
				return nil, err
			}
			return nil, graph.Commit(tx, build, types.Result{ID: types.ResultID(newID(t, "res")), Computation: v.State.Key.Key, Kind: types.ResultKindPatch,
				Value: []types.Ref{sealed}, Outcome: types.OutcomeFound, Reason: vocab.ReasonCompletedWithFindings, Detail: "the build produced a change", Completed: true}, x.Env)
		})
		w.view(func(tx *eventstore.Tx) error {
			recs, err := intent.Load(tx, w.blobs, task)
			if err != nil {
				return err
			}
			call := types.ManifestIntent{Consumer: types.ManifestIntentConsumer{Role: "builder"}}
			_, err = intent.Builder(intent.In{Call: call, Context: graph.Rendered{Included: []types.Ref{sealed}}, Bytes: map[types.Digest]int64{sealed.IdentityDigest: 7}, Records: recs})
			if !errors.Is(err, intent.ErrNotTheBrief) {
				t.Errorf("a forged plan ref as an ancestor: %v", err)
			}
			return nil
		})
	})
}

// forgedPlan admits a task, records forge's plan result in place of the
// loop's brief, and lets the loop claim the build on it (no planner, no
// placer): the store, the executor, the task and the build.
func forgedPlan(t *testing.T, forge forgery) (*seeder, reconcile.Executor, types.TaskID, types.NodeID) {
	t.Helper()
	perDay := int64(1 << 50)
	w, sys, x, _ := placing(t, &perDay)
	x.Planner, x.Placer = nil, nil // the test records the plan's result; nothing is placed
	_, task := readyTask(t, w, sys)
	l := reconcile.NewLoop(x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	passes(t, l)
	plan := templateNodes(w, task)[types.NodeKindPlan]
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		rec, err := authority.TaskOf(tx, task)
		if err != nil {
			return nil, err
		}
		raw, _, err := authority.ProjectDataPolicy(tx, graphProject)
		if err != nil {
			return nil, err
		}
		policy, err := brief.Restriction(graphProject, raw)
		if err != nil {
			return nil, err
		}
		b, ref := forge(t, rec, policy)
		if _, err := w.blobs.Put(bytes.NewReader(b), string(ref.ContentDigest)); err != nil {
			return nil, err
		}
		sealed, err := knowledge.Seal(ref)
		if err != nil {
			return nil, err
		}
		v, err := graph.ViewOf(tx, plan)
		if err != nil {
			return nil, err
		}
		return nil, graph.Commit(tx, plan, types.Result{ID: types.ResultID(newID(t, "res")), Computation: v.State.Key.Key, Kind: types.ResultKindPlan,
			Value: []types.Ref{sealed}, Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Completed: true}, x.Env)
	})
	passes(t, l)
	return w, x, task, templateNodes(w, task)[types.NodeKindBuild]
}

// Opus 22a-5c-3 r2 L1: a project record the store cannot decode is Load's
// own error, never a deferred "no data policy" that only a plan node's ref
// would meet.
func TestAnUnreadableProfileIsLoadsError(t *testing.T) {
	perDay := int64(1 << 50)
	w, sys, _, _ := placing(t, &perDay)
	_, task := readyTask(t, w, sys)
	rollback := errors.New("rolled back")
	var loadErr error
	err := w.try(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		if err := tx.Put(authority.ProjectProjection, string(graphProject), json.RawMessage(`{"profile":"not a profile"}`)); err != nil {
			return nil, err
		}
		_, loadErr = intent.Load(tx, w.blobs, task)
		return nil, rollback
	})
	if !errors.Is(err, rollback) || loadErr == nil || errors.Is(loadErr, intent.ErrNotTheBrief) {
		t.Errorf("Load over an unreadable profile: %v (the command: %v)", loadErr, err)
	}
}
