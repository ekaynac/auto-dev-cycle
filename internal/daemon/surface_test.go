// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
)

// asSystem submits decide as a system command of adcd's store.
func asSystem(t *testing.T, d *Daemon, decide func(*eventstore.Tx) ([]eventstore.Event, error)) error {
	t.Helper()
	cmd := newID(t, "cmd")
	_, err := d.store.SubmitApplied(context.Background(), d.system.Command(cmd, modelcall.Action, cmd, []byte(`{}`), nil),
		eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := decide(tx)
			return evs, json.RawMessage(`{}`), err
		}))
	return err
}

// recordedAs is the node.recorded of a plan node, its kind then set to
// kind; an evidence node is a valid one (graph.Record checks it).
func recordedAs(t *testing.T, kind types.NodeKind) func(*eventstore.Tx) ([]eventstore.Event, error) {
	return func(*eventstore.Tx) ([]eventstore.Event, error) {
		n := types.Node{ID: types.NodeID(newID(t, "nod")), Kind: types.NodeKindPlan, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`),
			Inputs: []types.InputSpec{}, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}
		if kind == types.NodeKindEvidence {
			n.Kind, n.Requires.Resources = kind, []types.ResourceGroupKind{types.ResourceGroupKindEvidenceTarget}
			n.Inputs = []types.InputSpec{{Name: "tree", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: types.NodeID(newID(t, "nod")), ResultKind: types.ResultKindTree}}}}
			n.Params = json.RawMessage(`{"instrument":{"name":"walk","version":"1","digest":"sha256:` + strings.Repeat("a", 64) + `"},` +
				`"grant":"apr_01m3250v0aer9ss2097svmefj8","target":"tgt_01m3250v0aer9ss2097svmefj8","subject_input":"tree","demand":{"key":"sha256:` + strings.Repeat("b", 64) + `"},` +
				`"limits":{"max_attempts_per_demand":1,"max_attempts_per_subject":10,"deadline":"PT1H","cooldown":"PT0S"},"restore":{"required":false,"procedure":null}}`)
		}
		evs, err := graph.Record(types.NodeRecorded{Task: types.TaskID(newID(t, "tsk")), Project: graphProject, Node: n})
		if err == nil && kind != n.Kind {
			evs[0].Payload = bytes.Replace(evs[0].Payload, []byte(`"kind":"plan"`), []byte(`"kind":"`+string(kind)+`"`), 1)
		}
		return evs, err
	}
}

// P22a-2 (I15 admission, Opus #298 M2, L3): adcd's graph fold refuses a
// node.recorded of a kind outside M1(a)'s (evidence, merge before merge is
// active), whoever records it; and IntendEffect refuses merge, which no
// active capability registers (model_call is launch's, active: P22a).
func TestDeferredSurfacesInAdcd(t *testing.T) {
	d := restart(t, stateDir(t))
	defer d.Close()
	for _, k := range []types.NodeKind{types.NodeKindEvidence, types.NodeKindClosure, types.NodeKindRecord, types.NodeKindPhaseGate, types.NodeKindRebase, types.NodeKindCarryOver} {
		if err := asSystem(t, d, recordedAs(t, k)); err == nil || !strings.Contains(err.Error(), "deferred surface: a "+string(k)+" node") {
			t.Errorf("a %s node recorded: %v", k, err)
		}
	}
	// An M1(a) kind passes the guard: it is refused only as a node of a task
	// that is not admitted (P22a-3).
	if err := asSystem(t, d, recordedAs(t, types.NodeKindBuild)); err == nil || strings.Contains(err.Error(), "deferred surface") || !strings.Contains(err.Error(), "not admitted") {
		t.Errorf("a build node: %v", err)
	}
	for _, kind := range []string{"merge"} {
		err := asSystem(t, d, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return nil, tx.IntendEffect(newID(t, "eff"), kind, "target", json.RawMessage(`{}`))
		})
		if err == nil || !strings.Contains(err.Error(), "unregistered effect kind") {
			t.Errorf("IntendEffect(%s): %v", kind, err)
		}
	}
}
