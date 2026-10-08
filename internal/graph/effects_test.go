// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/effecttarget"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// openEffectStore is openStore whose commands may intend deploy (idempotent
// at the target) and merge (reconciled first) effects.
func openEffectStore(t *testing.T) graphStore {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	lock, err := eventstore.AcquireLock(state)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	none := func(*eventstore.Tx, eventstore.Command) error { return nil }
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: Kinds(), Blobs: blobs,
		Effects: eventstore.EffectKinds{"deploy": {Idempotent: true}, "merge": {}},
		Hooks:   eventstore.Hooks{Identify: none, Authorize: none, Project: eventstore.ProjectEach(Project)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); lock.Release() })
	g := graphStore{t: t, s: s, blobs: blobs, conclusions: map[types.Digest]json.RawMessage{}}
	g.plan, g.planRes = g.planOf(tskID, prjID)
	return g
}

var effectEnv = Env{Need: noRequirement}

// pkgOf completes an integration node whose package result holds refs.
func (g graphStore) pkgOf(refs ...types.Ref) (types.NodeID, types.Result) {
	return g.completedWith(types.NodeKindIntegration, types.ResultKindPackage, append([]types.Ref{}, refs...))
}

// pkg is pkgOf one blob, of content.
func (g graphStore) pkg(content string) (types.NodeID, types.Result) {
	return g.pkgOf(g.sealed(types.RefTypeBlob, "package", g.blob(content)))
}

// effectOn records a node of kind with effect and params taking pkg's
// result, and claims it unless unclaimed.
func (g graphStore) effectOn(pkg types.NodeID, kind types.NodeKind, effect, params string, claimed bool) types.NodeID {
	g.t.Helper()
	rec := g.newNode(kind)
	rec.Node.Effect, rec.Node.Params = ptr(types.Name(effect)), json.RawMessage(params)
	rec.Node.Inputs = []types.InputSpec{{Name: "package", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: pkg, ResultKind: types.ResultKindPackage}}}}
	n := g.record(rec)
	if claimed {
		res := *g.state(pkg).Result
		g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return Claim(tx, n, map[string]Resolved{"package": {Result: res}}, 0)
		})
	}
	return n
}

// deployTo is a claimed deploy of a new package of content to target.
func (g graphStore) deployTo(content, target string) types.NodeID {
	pkg, _ := g.pkg(content)
	return g.effectOn(pkg, types.NodeKindDeploy, "deploy", `{"target":"`+target+`"}`, true)
}

func (g graphStore) operate(n types.NodeID) (op Operation, err error) {
	err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		var oerr error
		op, oerr = Operate(tx, n, effectEnv)
		return nil, oerr
	})
	return op, err
}

func (g graphStore) mustOperate(n types.NodeID) Operation {
	g.t.Helper()
	op, err := g.operate(n)
	if err != nil {
		g.t.Fatal(err)
	}
	return op
}

func (g graphStore) effect(id string) eventstore.Effect {
	g.t.Helper()
	e, err := g.s.LoadEffect(context.Background(), id)
	if err != nil {
		g.t.Fatal(err)
	}
	return e
}

func (g graphStore) uncertain() {
	g.t.Helper()
	actor, _ := ids.New("act")
	if _, err := g.s.MarkPendingUncertain(context.Background(), actor); err != nil {
		g.t.Fatal(err)
	}
}

func (g graphStore) runEffect(op Operation, target EffectTarget) (eventstore.Effect, error) {
	actor, _ := ids.New("act")
	return RunEffect(context.Background(), g.s, actor, op, target)
}

// eventsAbout is the number of events about subject.
func (g graphStore) eventsAbout(subject string) (n int) {
	g.t.Helper()
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) error {
		evs, err := tx.EventsAfter(0, 10000)
		for _, e := range evs {
			if e.Subject == subject {
				n++
			}
		}
		return err
	}); err != nil {
		g.t.Fatal(err)
	}
	return n
}

// Each effect node instance is its own operation, intended once: a repeat
// finds it. Two instances with one computation key are two operations, and
// a node's next generation is a new one; the want is the package's bytes.
func TestOperateKeysByNodeInstance(t *testing.T) {
	g := openEffectStore(t)
	tg := effecttarget.New()
	pkg, res := g.pkg("A")
	a1 := g.effectOn(pkg, types.NodeKindDeploy, "deploy", `{"target":"prod","strategy":"rolling"}`, true)
	op := g.mustOperate(a1)
	if again := g.mustOperate(a1); again.Effect != op.Effect {
		t.Fatalf("a repeat operated %s, want %s", again.Effect, op.Effect)
	}
	if e := g.effect(op.Effect); e.Kind != "deploy" || e.Target != "prod" || e.State != eventstore.EffectPending {
		t.Fatalf("effect %+v", e)
	}
	if op.Request.Want != res.Value[0].ContentDigest {
		t.Fatalf("the want is %s, not the package's content digest", op.Request.Want)
	}
	if _, err := g.runEffect(op, tg); err != nil {
		t.Fatal(err)
	}
	a2 := g.effectOn(pkg, types.NodeKindDeploy, "deploy", `{"target":"prod","strategy":"rolling"}`, true)
	if g.state(a1).Key.Key != g.state(a2).Key.Key {
		t.Fatal("the two deploys of one input do not share a computation key")
	}
	if op2 := g.mustOperate(a2); op2.Effect == op.Effect || op2.Request.Want != op.Request.Want {
		t.Fatalf("the second instance operated %+v; want a new effect wanting the same", op2)
	}
	if _, err := g.runEffect(g.mustOperate(a2), tg); err != nil {
		t.Fatal(err)
	}
	g.must(invalidate(a1))
	if op3 := g.mustOperate(a1); op3.Effect == op.Effect {
		t.Fatalf("the next generation resumed the first operation %s", op.Effect)
	}
}

func TestOperateRefusals(t *testing.T) {
	g := openEffectStore(t)
	pkg, _ := g.pkg("A")
	empty, _ := g.pkgOf()
	two, _ := g.pkgOf(g.sealed(types.RefTypeBlob, "a", g.blob("a")), g.sealed(types.RefTypeBlob, "b", g.blob("b")))
	held := g.effectOn(pkg, types.NodeKindDeploy, "deploy", `{"target":"prod"}`, true)
	g.must(raised(held, awaitingInput(t), nil))
	for name, n := range map[string]types.NodeID{
		"unclaimed":     g.effectOn(pkg, types.NodeKindDeploy, "deploy", `{"target":"prod"}`, false),
		"held":          held,
		"no target":     g.effectOn(pkg, types.NodeKindDeploy, "deploy", `{}`, true),
		"blank target":  g.effectOn(pkg, types.NodeKindDeploy, "deploy", `{"target":""}`, true),
		"not an effect": func() types.NodeID { n := g.record(g.newNode(types.NodeKindBuild)); g.must(g.claimOf(n)); return n }(),
		"not a node":    nodNil,
		"unregistered":  g.effectOn(pkg, types.NodeKindRecord, "notification", `{"target":"ops"}`, true),
		"no ref":        g.effectOn(empty, types.NodeKindDeploy, "deploy", `{"target":"prod"}`, true),
		"two refs":      g.effectOn(two, types.NodeKindDeploy, "deploy", `{"target":"prod"}`, true),
		"no result input": func() types.NodeID {
			rec := g.newNode(types.NodeKindDeploy)
			rec.Node.Effect, rec.Node.Params, rec.Node.Inputs = ptr(types.Name("deploy")), json.RawMessage(`{"target":"prod"}`), []types.InputSpec{}
			n := g.record(rec)
			g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Claim(tx, n, map[string]Resolved{}, 0) })
			return n
		}(),
	} {
		_, err := g.operate(n)
		if err == nil {
			t.Errorf("%s: operated", name)
		}
		if strings.HasSuffix(name, "target") && !strings.Contains(err.Error(), "params name its target") {
			t.Errorf("%s: refused for another reason: %v", name, err)
		}
	}
}

// One operation is live per target: another waits, typed, intending
// nothing, until the live one completes; another target is free.
func TestOperateFencesTheTarget(t *testing.T) {
	g := openEffectStore(t)
	a := g.mustOperate(g.deployTo("A", "prod"))
	b := g.deployTo("B", "prod")
	_, err := g.operate(b)
	var busy ErrTargetBusy
	if !errors.As(err, &busy) || busy.Live != a.Effect || busy.Target != "prod" {
		t.Fatalf("a second operation on a live target: %v", err)
	}
	g.uncertain()
	if _, err := g.operate(b); !errors.As(err, &busy) {
		t.Fatalf("an uncertain effect did not fence its target: %v", err)
	}
	g.mustOperate(g.deployTo("C", "staging"))
	if _, err := g.runEffect(a, effecttarget.New()); err != nil {
		t.Fatal(err)
	}
	g.mustOperate(b)
}

// A new operation re-validates its claimed inputs before its intent, as a
// commit does; a resume of an intended effect does not.
func TestOperateRevalidates(t *testing.T) {
	g := openEffectStore(t)
	pkg, _ := g.pkg("A")
	d := g.effectOn(pkg, types.NodeKindDeploy, "deploy", `{"target":"prod"}`, true)
	g.must(invalidate(pkg))
	_, err := g.operate(d)
	var stale ErrRevalidated
	if !errors.As(err, &stale) || stale.Reason != vocab.ReasonStaleExpectedState || stale.Input != "package" {
		t.Fatalf("an operation on an invalidated package: %v", err)
	}
	pkg2, _ := g.pkg("B")
	d2 := g.effectOn(pkg2, types.NodeKindDeploy, "deploy", `{"target":"staging"}`, true)
	op := g.mustOperate(d2)
	g.must(invalidate(pkg2))
	if resumed := g.mustOperate(d2); resumed.Effect != op.Effect {
		t.Fatalf("the resume operated %s, want %s", resumed.Effect, op.Effect)
	}
}

// Two recorded effects for one request are could_not_run/ambiguous_state.
func TestOperateAmbiguous(t *testing.T) {
	g := openEffectStore(t)
	intend := func(request any, target string) string {
		b, _ := json.Marshal(request)
		id, _ := ids.New("eff")
		g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return nil, tx.IntendEffect(id, "deploy", target, b)
		})
		return id
	}
	isAmbiguous := func(err error) bool {
		var amb ErrAmbiguousEffect
		return errors.As(err, &amb) && amb.Result().Reason == vocab.ReasonAmbiguousState && amb.Result().Outcome == vocab.CouldNotRun
	}
	// Two settled effects for the node's request.
	d := g.deployTo("A", "prod")
	op := g.mustOperate(d)
	dup := intend(op.Request, "prod")
	actor, _ := ids.New("act")
	for _, id := range []string{op.Effect, dup} {
		if _, err := g.s.RecordEffect(context.Background(), actor, id, eventstore.Transition{Kind: "observed"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.operate(d); !isAmbiguous(err) {
		t.Fatalf("two effects for one request: %v", err)
	}
	// Two live effects on the target of a new operation.
	intend(map[string]string{"a": "1"}, "staging")
	intend(map[string]string{"a": "2"}, "staging")
	if _, err := g.operate(g.deployTo("B", "staging")); !isAmbiguous(err) {
		t.Fatalf("two live effects on one target: %v", err)
	}
}

type failingTarget struct {
	*effecttarget.Target
	holds, apply error
}

func (f failingTarget) Holds(target string) (string, error) {
	if f.holds != nil {
		return "", f.holds
	}
	return f.Target.Holds(target)
}

func (f failingTarget) Apply(string, string, string, string) (bool, error) { return false, f.apply }

// RunEffect reconciles against the target's current state, applies by
// compare-and-set under the operation's effect id, and resumes an uncertain
// effect without intending another.
func TestRunEffect(t *testing.T) {
	g := openEffectStore(t)
	tg := effecttarget.New()
	run := func(op Operation, target EffectTarget) eventstore.Effect {
		t.Helper()
		e, err := g.runEffect(op, target)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	deploy := func(content, target string) Operation { return g.mustOperate(g.deployTo(content, target)) }

	// Pending: applied, then observed; a completed effect stays as it is.
	op := deploy("A", "prod")
	if e := run(op, tg); e.State != eventstore.EffectCompleted || e.LastObservation != "applied" {
		t.Fatalf("pending: %+v", e)
	}
	if e := run(op, tg); e.State != eventstore.EffectCompleted || len(tg.Applied("prod")) != 1 {
		t.Fatalf("completed again: %+v, applied %v", e, tg.Applied("prod"))
	}
	// A new operation whose target already holds its want: no egress.
	if e := run(deploy("A", "prod"), tg); e.State != eventstore.EffectCompleted || len(tg.Applied("prod")) != 1 {
		t.Fatalf("already held: %+v, applied %v", e, tg.Applied("prod"))
	}

	// Uncertain, the target not holding the want: an idempotent kind is
	// retried under its id.
	op = deploy("S", "staging")
	g.uncertain()
	if e := run(op, tg); e.State != eventstore.EffectCompleted || e.Attempts != 2 || !slices.Equal(tg.Applied("staging"), []string{op.Effect}) {
		t.Fatalf("uncertain, retried: %+v, applied %v", e, tg.Applied("staging"))
	}
	// Uncertain, the target holding the want: reconciled, no second apply.
	op = deploy("S", "staging")
	g.uncertain()
	if e := run(op, tg); e.State != eventstore.EffectCompleted || e.Attempts != 1 || len(tg.Applied("staging")) != 1 {
		t.Fatalf("uncertain, held: %+v, applied %v", e, tg.Applied("staging"))
	}

	// An operation runs only the effect intended for its request and kind.
	op = deploy("D", "dr")
	for name, forge := range map[string]func(*Operation){
		"target": func(o *Operation) { o.Request.Target = "staging" },
		"kind":   func(o *Operation) { o.Kind = "merge" },
	} {
		forged := op
		forge(&forged)
		if _, err := g.runEffect(forged, tg); err == nil || !strings.Contains(err.Error(), "another request") {
			t.Fatalf("an operation with another %s ran: %v", name, err)
		}
	}
	// A failing Holds changes nothing; a failing apply leaves it uncertain.
	if _, err := g.runEffect(op, failingTarget{tg, errors.New("unreachable"), nil}); err == nil || g.effect(op.Effect).State != eventstore.EffectPending {
		t.Fatalf("a failing Holds: %v, %s", err, g.effect(op.Effect).State)
	}
	if _, err := g.runEffect(op, failingTarget{tg, nil, errors.New("target unreachable")}); err == nil || !strings.Contains(err.Error(), "target unreachable") || g.effect(op.Effect).State != eventstore.EffectUncertain {
		t.Fatalf("a failed apply: %v", err)
	}
	// An apply that finds the target moved lands nothing: uncertain.
	if _, err := g.runEffect(op, failingTarget{tg, nil, nil}); !errors.As(err, new(ErrTargetMoved)) || g.effect(op.Effect).State != eventstore.EffectUncertain {
		t.Fatalf("a moved target: %v", err)
	}
	run(op, tg)

	// A kind reconciled first is never blindly retried, and a repeat that
	// observes nothing new records nothing.
	pkg, _ := g.pkg("M")
	merge := g.mustOperate(g.effectOn(pkg, types.NodeKindMerge, "merge", `{"target":"main"}`, true))
	g.uncertain()
	if e := run(merge, tg); e.State != eventstore.EffectUncertain || len(tg.Applied("main")) != 0 {
		t.Fatalf("merge: %+v, applied %v", e, tg.Applied("main"))
	}
	before := g.eventsAbout(merge.Effect)
	if e := run(merge, tg); e.State != eventstore.EffectUncertain || g.eventsAbout(merge.Effect) != before {
		t.Fatalf("a repeated reconciliation recorded %d events, want %d", g.eventsAbout(merge.Effect), before)
	}
}
