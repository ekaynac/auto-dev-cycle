// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/effecttarget"
)

// The intent is recorded with node.operated, the node's link to its effect,
// once; the kept request blob holds the request, so a reconciler needs only
// the effect's request_digest.
func TestOperatedLinksTheNode(t *testing.T) {
	g := openEffectStore(t)
	d := g.deployTo("A", "prod")
	op := g.mustOperate(d)
	g.mustOperate(d)
	if got, ok := view[types.NodeOperated](g, EffectProjection, string(d)); !ok || string(got.Effect) != op.Effect || got.Key != *g.state(d).Key {
		t.Fatalf("the node's operation is %+v (%v), want effect %s", got, ok, op.Effect)
	}
	if n := g.eventsAbout(string(d)); n != 3 { // recorded, claimed, operated
		t.Fatalf("the node has %d events, want 3", n)
	}
	rc, err := g.blobs.Open(g.effect(op.Effect).RequestDigest)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	var kept EffectRequest
	if err := json.Unmarshal(b, &kept); err != nil || kept.Want != op.Request.Want || kept.Node != d || kept.Target != "prod" {
		t.Fatalf("the kept request is %+v (%v)", kept, err)
	}
}

// A kind that keeps its request is not intended before the request is
// published.
func TestOperateNeedsTheKeptRequest(t *testing.T) {
	g := openEffectStore(t)
	d := g.deployTo("A", "prod")
	err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		_, err := Operate(tx, d, effectEnv)
		return nil, err
	})
	if err == nil || !strings.Contains(err.Error(), "is not a published blob") {
		t.Fatalf("an unpublished request was intended: %v", err)
	}
}

func TestOperatedFoldRefusals(t *testing.T) {
	g := openEffectStore(t)
	d := g.deployTo("A", "prod")
	op := g.mustOperate(d)
	key := *g.state(d).Key
	other := key
	other.Generation++
	eff, _ := ids.New("eff")
	pkg, _ := g.pkg("B")
	unclaimed := g.effectOn(pkg, types.NodeKindDeploy, "deploy", `{"target":"prod"}`, false)
	build := g.record(g.newNode(types.NodeKindBuild))
	g.must(g.claimOf(build))
	// A claimed deploy to staging, not operated, and effects to link it to.
	fresh := g.deployTo("C", "staging")
	held := g.deployTo("D", "prod")
	g.must(raised(held, awaitingInput(t), nil))
	intended := func(kind, target string) types.EffectID {
		id, _ := ids.New("eff")
		b := []byte(`{"forged":"` + id + `"}`)
		if _, err := g.blobs.Put(bytes.NewReader(b), ""); err != nil {
			t.Fatal(err)
		}
		g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return nil, tx.IntendEffect(id, kind, target, b) })
		return types.EffectID(id)
	}
	elsewhere, merge := intended("deploy", "dr"), intended("merge", "staging")
	for name, ev := range map[string]*types.NodeOperated{
		"the same key again":        {Node: d, Key: key, Effect: types.EffectID(op.Effect)},
		"another key":               {Node: d, Key: other, Effect: types.EffectID(op.Effect)},
		"a held node":               {Node: held, Key: *g.state(held).Key, Effect: types.EffectID(op.Effect)},
		"unclaimed":                 {Node: unclaimed, Key: key, Effect: types.EffectID(eff)},
		"not an effect node":        {Node: build, Key: *g.state(build).Key, Effect: types.EffectID(eff)},
		"no such effect":            {Node: fresh, Key: *g.state(fresh).Key, Effect: types.EffectID(eff)},
		"an effect elsewhere":       {Node: fresh, Key: *g.state(fresh).Key, Effect: elsewhere},
		"an effect of another kind": {Node: fresh, Key: *g.state(fresh).Key, Effect: merge},
	} {
		ev.Type = "node_operated"
		if err := g.do(forge(NodeOperated, string(ev.Node), prjID, ev)); err == nil {
			t.Errorf("%s: folded", name)
		}
	}

	// An invalidation of a node whose effect is live is refused by the
	// fold too, and admitted once the effect settles.
	rec, _ := view[nodeRecord](g, NodeProjection, string(d))
	c, _ := view[claim](g, ClaimProjection, string(d))
	next, err := Key(rec.Recorded.Node, resolutions(c.Inputs), c.Key.Generation+1)
	if err != nil {
		t.Fatal(err)
	}
	forged := forge(NodeInvalidated, string(d), prjID, &types.NodeInvalidated{Type: "node_invalidated", Node: d, Key: next})
	var live ErrEffectLive
	if err := g.do(forged); !errors.As(err, &live) {
		t.Fatalf("a forged invalidation of a node with a live effect: %v", err)
	}
	if _, err := g.runEffect(op, effecttarget.New()); err != nil {
		t.Fatal(err)
	}
	g.must(forged)
}

// Operate runs only in a command's transaction.
func TestOperateNeedsACommand(t *testing.T) {
	g := openEffectStore(t)
	d := g.deployTo("A", "prod")
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) error { _, err := Operate(tx, d, effectEnv); return err }); err == nil || !strings.Contains(err.Error(), "command's transaction") {
		t.Fatalf("an operation in a view: %v", err)
	}
}

// An effect node is not invalidated while its effect is live, and an
// operation runs only its node's current request.
func TestInvalidateWaitsForTheEffect(t *testing.T) {
	g := openEffectStore(t)
	d := g.deployTo("A", "prod")
	op := g.mustOperate(d)
	var live ErrEffectLive
	if err := g.do(invalidate(d)); !errors.As(err, &live) || live.Effect != op.Effect {
		t.Fatalf("invalidating a node with a pending effect: %v", err)
	}
	g.uncertain()
	if err := g.do(invalidate(d)); !errors.As(err, &live) {
		t.Fatalf("invalidating a node with an uncertain effect: %v", err)
	}
	if _, err := g.runEffect(op, effecttarget.New()); err != nil {
		t.Fatal(err)
	}
	g.must(invalidate(d))
	if _, err := g.runEffect(op, effecttarget.New()); err == nil || !strings.Contains(err.Error(), "current one") {
		t.Fatalf("an operation of a node's earlier generation ran: %v", err)
	}
}

// No node takes a deploy's result until G7b defines effect-node completion.
func TestRecordRefusesADeployInput(t *testing.T) {
	g := openEffectStore(t)
	d := g.deployTo("A", "prod")
	rec := g.newNode(types.NodeKindBuild)
	rec.Node.Inputs = []types.InputSpec{{Name: "deployed", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: d, ResultKind: types.ResultKindPackage}}}}
	if err := g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return Record(rec) }); err == nil || !strings.Contains(err.Error(), "deploy's result") {
		t.Fatalf("a node taking a deploy's result was recorded: %v", err)
	}
	// A consumer recorded before its deploy producer: the deploy is refused.
	id, _ := ids.New("nod")
	consumer := g.newNode(types.NodeKindBuild)
	consumer.Node.Inputs = []types.InputSpec{{Name: "deployed", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: types.NodeID(id), ResultKind: types.ResultKindPackage}}}}
	g.record(consumer)
	pkg, _ := g.pkg("B")
	deploy := g.newNode(types.NodeKindDeploy)
	deploy.Node.ID, deploy.Node.Effect, deploy.Node.Params = types.NodeID(id), ptr(types.Name("deploy")), json.RawMessage(`{"target":"prod"}`)
	deploy.Node.Inputs = []types.InputSpec{{Name: "package", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: pkg, ResultKind: types.ResultKindPackage}}}}
	if err := g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return Record(deploy) }); err == nil || !strings.Contains(err.Error(), "deploy's result") {
		t.Fatalf("a deploy an earlier node takes was recorded: %v", err)
	}
}
