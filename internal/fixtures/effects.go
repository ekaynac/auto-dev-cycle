// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/effecttarget"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// deployTarget is the target every fixture deploy acts on.
const deployTarget = "prod"

// effectFix is a graph fixture whose store intends deploy effects (an
// idempotent kind at its target, 03), landing on the fake effect target,
// which outlives a restart.
type effectFix struct {
	*graphFix
	dir, actor string
	close      func() error
	target     *effecttarget.Target
}

// open opens the store in dir.
func (f *effectFix) open() error {
	state := filepath.Join(f.dir, "state")
	lock, err := eventstore.AcquireLock(state)
	if err != nil {
		return err
	}
	blobs, err := blobstore.Open(state)
	none := func(*eventstore.Tx, eventstore.Command) error { return nil }
	var s *eventstore.Store
	if err == nil {
		s, err = eventstore.Open(f.ctx, lock, eventstore.Config{Kinds: graph.Kinds(), Blobs: blobs,
			Effects: eventstore.EffectKinds{"deploy": {Idempotent: true}},
			Hooks:   eventstore.Hooks{Identify: none, Authorize: none, Project: eventstore.ProjectEach(graph.Project)}})
	}
	if err != nil {
		return errors.Join(err, lock.Release())
	}
	f.graphFix = &graphFix{ctx: f.ctx, s: s, blobs: blobs, actor: f.actor}
	f.close = func() error { return errors.Join(s.Close(), lock.Release()) }
	return nil
}

func newEffectFix(ctx context.Context, dir string) (*effectFix, error) {
	actor, err := ids.New("act")
	f := &effectFix{graphFix: &graphFix{ctx: ctx}, dir: dir, actor: actor, target: effecttarget.New()}
	if err == nil {
		err = f.open()
	}
	return f, err
}

func (f *effectFix) env() graph.Env {
	return graph.Env{Need: noNeed, Blobs: f.blobs, Authority: owedanswers.Authority{}}
}

// restart closes the store as a crash leaves it and reopens it; the restart
// rule makes the pending effect uncertain (03).
func (f *effectFix) restart() error {
	err := f.close()
	if err == nil {
		err = f.open()
	}
	if n, merr := f.s.MarkPendingUncertain(f.ctx, f.actor); err == nil && (merr != nil || n != 1) {
		err = errors.Join(merr, fmt.Errorf("the restart found %d pending effects, want 1", n))
	}
	return err
}

// pkg completes an integration node whose package result holds one blob,
// name's package.
func (f *effectFix) pkg(name string) (types.NodeID, error) {
	d, err := f.blobs.Put(strings.NewReader("package "+name+"\n"), "")
	if err != nil {
		return "", err
	}
	ref, err := knowledge.Seal(types.Ref{Type: types.RefTypeBlob, ID: "package-" + name, Scope: types.RefScope{Project: ptrTo(gProject)},
		Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: types.Digest(d)})
	if err != nil {
		return "", err
	}
	n, _, err := f.complete(types.NodeKindIntegration, json.RawMessage(`{"package":"`+name+`"}`), []types.InputSpec{},
		map[string]graph.Resolved{}, types.ResultKindPackage, []types.Ref{ref}, nil)
	return n, err
}

// goal records a new node deploying pkg's package to deployTarget, and
// claims it by evaluation.
func (f *effectFix) goal(pkg types.NodeID) (types.NodeID, error) {
	nid, err := ids.New("nod")
	n := types.NodeRecorded{Type: "node_recorded", Task: gTask, Project: gProject, Node: types.Node{ID: types.NodeID(nid), Kind: types.NodeKindDeploy,
		DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{"target":"` + deployTarget + `"}`), Effect: ptrTo(types.Name("deploy")),
		Inputs:   []types.InputSpec{{Name: "package", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: pkg, ResultKind: types.ResultKindPackage}}}},
		Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}
	if err == nil {
		err = f.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return graph.Record(n) })
	}
	if err == nil {
		err = f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Evaluate(tx, []types.NodeID{n.Node.ID}, f.env())
		})
	}
	return n.Node.ID, err
}

// operate is node's effect operation, in a command.
func (f *effectFix) operate(node types.NodeID) (op graph.Operation, err error) {
	err = f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		var oerr error
		op, oerr = graph.Operate(tx, node, f.env())
		return nil, oerr
	})
	return op, err
}

// run carries op to target: a completed effect is the step
// empty/completed_clean, any other state the fixture not running.
func (f *effectFix) run(o *observe, op graph.Operation, target graph.EffectTarget) error {
	e, err := graph.RunEffect(f.ctx, f.s, f.actor, op, target)
	if err == nil && e.State != eventstore.EffectCompleted {
		err = fmt.Errorf("effect %s is %s, not completed", e.ID, e.State)
	}
	if err == nil {
		o.steps = append(o.steps, Step{vocab.Empty, vocab.ReasonCompletedClean.String()})
	}
	return err
}

// deployGoal records, claims, operates and runs a goal deploying pkg.
func (f *effectFix) deployGoal(o *observe, pkg types.NodeID) (graph.Operation, error) {
	n, err := f.goal(pkg)
	op, oerr := f.operate(n)
	if err = errors.Join(err, oerr); err == nil {
		err = f.run(o, op, f.target)
	}
	return op, err
}

// holds is what deployTarget holds.
func (f *effectFix) holds() string { h, _ := f.target.Holds(deployTarget); return h }

// deployAgainDeploysAgain: deploy A, then B, then A again deploys A again
// (13 [G4]). Each goal is a new deploy node and so a new effect operation,
// reconciled against the target's current state, never an earlier
// operation's result; a fourth goal, A while the target holds A, applies
// nothing.
func deployAgainDeploysAgain(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	f, err := newEffectFix(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, f.close()) }()
	pkgA, err := f.pkg("A")
	pkgB, berr := f.pkg("B")
	if err = errors.Join(err, berr); err != nil {
		return nil, nil, err
	}
	var o observe
	var ops []graph.Operation
	for _, pkg := range []types.NodeID{pkgA, pkgB, pkgA, pkgA} {
		op, err := f.deployGoal(&o, pkg)
		if err != nil {
			return nil, nil, err
		}
		o.check(f.holds() == string(op.Request.Want), "the target holds %s, not what the goal deploying %s wants", f.holds(), pkg)
		ops = append(ops, op)
	}
	o.check(ops[2].Effect != ops[0].Effect, "deploy A again resolved to the first deploy's operation %s", ops[0].Effect)
	want, got := []string{ops[0].Effect, ops[1].Effect, ops[2].Effect}, f.target.Applied(deployTarget)
	o.check(slices.Equal(got, want), "the target applied %v, want %v (the fourth goal finds A held)", got, want)
	return o.steps, o.violations, nil
}

// crashAfterIntentResumes: a crash after an effect's intent resumes the
// effect and never allocates another (13 [G4], 03). After a crash before
// the send, the operation retries its intended effect under its id; after a
// crash between the send and its observation, it reconciles against the
// target, which holds what it wants, with no second apply.
func crashAfterIntentResumes(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	f, err := newEffectFix(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, f.close()) }()
	var o observe
	for i, sent := range []bool{false, true} {
		pkg, err := f.pkg(fmt.Sprint("P", i))
		n, gerr := f.goal(pkg)
		intended, oerr := f.operate(n)
		if err = errors.Join(err, gerr, oerr); err == nil && sent {
			_, err = f.target.Apply(deployTarget, intended.Effect, string(intended.Request.Want), f.holds())
		}
		before := len(f.target.Applied(deployTarget))
		if err == nil {
			err = f.restart()
		}
		var resumed graph.Operation
		if err == nil {
			resumed, err = f.operate(n)
		}
		if busy := (graph.ErrTargetBusy{}); errors.As(err, &busy) {
			o.check(false, "the resume was refused: %v", err)
			continue
		}
		if err == nil {
			err = f.run(&o, resumed, f.target)
		}
		e, lerr := f.s.LoadEffect(ctx, intended.Effect)
		if err = errors.Join(err, lerr); err != nil {
			return nil, nil, err
		}
		o.check(resumed.Effect == intended.Effect, "the resume ran effect %s, not the intended %s", resumed.Effect, intended.Effect)
		o.check(e.State == eventstore.EffectCompleted, "the intended effect %s is %s, not completed", e.ID, e.State)
		applies, want := len(f.target.Applied(deployTarget))-before, map[bool]int{false: 1, true: 0}[sent]
		o.check(applies == want, "the resume applied %d times, want %d", applies, want)
	}
	intents, err := f.count("effect.intent/v1")
	o.check(intents == 2, "two operations intended %d effects, want 2", intents)
	return o.steps, o.violations, err
}

// oneLiveOperationPerTarget: an operation waits while another effect on its
// target is live, so a stale operation never lands over a newer one (13
// [G4]). A is intended and the controller crashes; B is refused while A is
// uncertain; A resumes, then B applies, and the target ends on B.
func oneLiveOperationPerTarget(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	f, err := newEffectFix(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, f.close()) }()
	pkgA, err := f.pkg("A")
	pkgB, berr := f.pkg("B")
	a, aerr := f.goal(pkgA)
	b, gerr := f.goal(pkgB)
	opA, oerr := f.operate(a)
	if err = errors.Join(err, berr, aerr, gerr, oerr, f.restart()); err != nil {
		return nil, nil, err
	}
	var o observe
	opB, err := f.operate(b)
	var busy graph.ErrTargetBusy
	switch {
	case errors.As(err, &busy):
		o.check(busy.Live == opA.Effect, "B waited on %s, not A's %s", busy.Live, opA.Effect)
	case err == nil:
		o.check(false, "B was intended while A's effect %s was live", opA.Effect)
		err = f.run(&o, opB, f.target)
	}
	if err == nil || errors.As(err, &busy) {
		err = f.run(&o, opA, f.target) // A resumes
	}
	if err == nil {
		opB, err = f.operate(b)
	}
	if err == nil {
		err = f.run(&o, opB, f.target)
	}
	if err != nil {
		return nil, nil, err
	}
	o.check(f.holds() == string(opB.Request.Want), "the target ends on %s, not on B", f.holds())
	want, got := []string{opA.Effect, opB.Effect}, f.target.Applied(deployTarget)
	o.check(slices.Equal(got, want), "the target applied %v, want %v", got, want)
	return o.steps, o.violations, nil
}

// racingTarget moves the target once, right after the first Holds read: an
// external change between RunEffect's observation and its apply.
type racingTarget struct {
	*effecttarget.Target
	race func() error
}

func (r *racingTarget) Holds(target string) (string, error) {
	h, err := r.Target.Holds(target)
	if race := r.race; race != nil {
		r.race = nil
		err = errors.Join(err, race())
	}
	return h, err
}

// targetMovedRefusesApply: an apply is a compare-and-set on the value
// RunEffect observed (13 [G4]). The target moves to X between the
// observation and the apply, so A's apply does not land and its effect is
// uncertain; run again, it reconciles against X and applies A.
func targetMovedRefusesApply(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	f, err := newEffectFix(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, f.close()) }()
	pkgA, err := f.pkg("A")
	a, gerr := f.goal(pkgA)
	opA, oerr := f.operate(a)
	if err = errors.Join(err, gerr, oerr); err != nil {
		return nil, nil, err
	}
	var o observe
	racing := &racingTarget{Target: f.target, race: func() error {
		_, err := f.target.Apply(deployTarget, "external", "X", "")
		return err
	}}
	e, err := graph.RunEffect(ctx, f.s, f.actor, opA, racing)
	var moved graph.ErrTargetMoved
	if errors.As(err, &moved) {
		err = nil
	} else {
		o.check(false, "the apply over a moved target ended %s (%v)", e.State, err)
	}
	o.check(e.State == eventstore.EffectUncertain, "after the move the effect is %s, not uncertain", e.State)
	o.check(slices.Equal(f.target.Applied(deployTarget), []string{"external"}), "the moved target applied %v, want only the external change", f.target.Applied(deployTarget))
	if err == nil {
		err = f.run(&o, opA, f.target)
	}
	if err != nil {
		return nil, nil, err
	}
	o.check(f.holds() == string(opA.Request.Want), "the target ends on %s, not on A", f.holds())
	return o.steps, o.violations, nil
}
