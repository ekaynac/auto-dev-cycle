// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const graphProject = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sa1")

// heldNode records, through w, a plan node of a new task and raises
// source_invalid_config on it, the operator's hold. It returns the node.
func heldNode(t *testing.T, w *seeder, actor types.ActorID) types.NodeID {
	t.Helper()
	return heldNodeIn(t, w, actor, graphProject)
}

// heldNodeIn is heldNode, in project.
func heldNodeIn(t *testing.T, w *seeder, actor types.ActorID, project types.ProjectID) types.NodeID {
	t.Helper()
	n := types.Node{ID: types.NodeID(newID(t, "nod")), Kind: types.NodeKindPlan, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`),
		Inputs: []types.InputSpec{}, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}
	w.do(actor, func(*eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Record(types.NodeRecorded{Task: types.TaskID(newID(t, "tsk")), Project: project, Node: n})
	})
	w.do(actor, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Raise(tx, n.ID, types.NodeHold{ID: types.HoldID(newID(t, "hld")), Hold: "source_invalid_config", SignalInstance: "cfg@2",
			Owner: types.OwnerOperator, Pages: []types.NodeHoldPagesItem{}}, nil)
	})
	return n.ID
}

// graphStore is fullStore with a held graph node: every projector the
// daemon composes, the graph's included, holds a record.
func graphStore(t *testing.T) (string, Config) {
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t, tokens(10)))
	heldNode(t, w, fullHistory(t, w))
	w.done()
	return dir, Config{StateDir: dir, Groups: []types.ResourceGroup{tokens(10)}}
}

// The graph is wired: a store holding graph events starts, its graph
// projections equal to a replay, again after a restart; a graph projection
// edited behind replay is refused.
func TestGraphReplayedAtStart(t *testing.T) {
	dir, cfg := graphStore(t)
	if n := count(t, dir, `SELECT count(*) FROM proj_kv WHERE projection = '`+graph.NodeProjection+`'`); n != 1 {
		t.Fatalf("%d graph nodes projected", n)
	}
	for range 2 {
		d, err := Start(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
	execSQL(t, dir, `UPDATE proj_kv SET value = json_set(value, '$.forged', 1) WHERE projection = '`+graph.NodeProjection+`'`)
	refused(t, dir, cfg, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "differ from a replay")
}

// graph.CheckHistory runs on the existing projections before any replay:
// an operation naming no node or effect, which also drifts from a replay,
// is refused with the history's own diagnosis. Skipping the check fails
// this test (the refusal is then the replay's).
func TestGraphHistoryCheckedBeforeReplay(t *testing.T) {
	dir, cfg := graphStore(t)
	op, err := json.Marshal(types.NodeOperated{Type: "node_operated", Node: types.NodeID(newID(t, "nod")), Effect: types.EffectID(newID(t, "eff"))})
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, dir, `INSERT INTO proj_kv(projection, key, value) VALUES ('`+graph.EffectProjection+`', 'nod_forged', '`+string(op)+`')`)
	head := lastSeq(t, dir)
	refused(t, dir, cfg, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "the graph history disagrees")
	if lastSeq(t, dir) != head {
		t.Fatal("a refused start appended to the log")
	}
	execSQL(t, dir, `UPDATE proj_kv SET value = '1' WHERE projection = '`+graph.EffectProjection+`'`)
	refused(t, dir, cfg, vocab.CouldNotRun, vocab.ReasonStoreUnreadable, "projections cannot be read")
}

// adc holds list shows node and lease holds in one shape, to a reader of
// every project; a supervisor whose grant reads no holds sees none.
func TestHoldsList(t *testing.T) {
	var node types.NodeID
	c := runCommanded(t, func(w *seeder, op types.ActorID) {
		node = heldNode(t, w, op)
		w.heldLease(op)
		w.heldLease(op, graph.UnitOf(node)) // its unit is the node (M1 P8a): its hold has the node's project
	})
	code, out, errb := c.adc(c.op.Reveal(), "holds", "list", "--json")
	var list struct {
		Outcome string
		Items   []types.HoldRead
	}
	if code != entry.ExitOK || json.Unmarshal([]byte(out), &list) != nil || len(list.Items) != 3 {
		t.Fatalf("holds list: %d %s %s", code, out, errb)
	}
	n, l, bound := list.Items[0], list.Items[1], list.Items[2]
	if l.Project != nil {
		l, bound = bound, l
	}
	if bound.Held != types.HoldReadHeldLease || bound.Project == nil || *bound.Project != graphProject {
		t.Errorf("the hold of a lease on the node's unit: %+v", bound)
	}
	if n.Held != types.HoldReadHeldNode || n.Subject != types.TypeID(node) || n.ID == nil || n.Project == nil || *n.Project != graphProject ||
		n.Hold != "source_invalid_config" || n.Owner != types.OwnerOperator || n.Since == nil || n.Pages == nil {
		t.Errorf("the node hold: %+v", n)
	}
	if l.Held != types.HoldReadHeldLease || !strings.HasPrefix(string(l.Subject), "lea_") || l.ID != nil || l.Project != nil || l.Since != nil ||
		l.Hold != "quota_reset" || l.SignalInstance != "2026-10-04T15:00:00Z" || l.Owner != types.OwnerSystem || l.Pages == nil {
		t.Errorf("the lease hold: %+v", l)
	}
	if code, out, _ := c.adc(c.op.Reveal(), "holds", "list"); code != entry.ExitOK || !strings.Contains(out, "holds hld_") {
		t.Errorf("holds list: %d %s", code, out)
	}
	if code, out, _ := c.adc(c.sup.Reveal(), "holds", "list", "--json"); code != entry.ExitOK || !strings.Contains(out, `"items":[]`) {
		t.Errorf("a supervisor reading no holds: %d %s", code, out)
	}
}

// adc board reads the installed board: empty before any task; a node of
// no recorded task is listed unplaced, with no column, in its project.
func TestBoardCommand(t *testing.T) {
	c := runCommanded(t)
	if code, out, errb := c.adc(c.op.Reveal(), "board", "--json"); code != entry.ExitOK || !strings.Contains(out, `"resource":"board"`) ||
		!strings.Contains(out, `"outcome":"empty"`) {
		t.Fatalf("board: %d %s %s", code, out, errb)
	}
	if code, out, errb := c.adc(c.op.Reveal(), "board"); code != entry.ExitOK || !strings.Contains(out, "board: none readable") {
		t.Fatalf("board: %d %s %s", code, out, errb)
	}
	c = runCommanded(t, func(w *seeder, op types.ActorID) { heldNode(t, w, op) })
	code, out, errb := c.adc(c.op.Reveal(), "board", "--json")
	if code != entry.ExitOK || !strings.Contains(out, `"type":"unplaced"`) || !strings.Contains(out, `"reason":"task_unrecorded"`) ||
		!strings.Contains(out, `"project":"`+string(graphProject)+`"`) || strings.Contains(out, `"column"`) {
		t.Fatalf("a node of no task: %d %s %s", code, out, errb)
	}
}

// A lease hold on a node's unit is its node's project's (M1 P8a; #226
// entry 13): a reader scoped to that project sees it, one scoped to another
// project does not, and neither sees the hold of a lease bound to no node.
func TestLeaseHoldProjectIsolation(t *testing.T) {
	readers := map[types.ProjectID]identity.Secret{}
	c := runCommanded(t, func(w *seeder, op types.ActorID) {
		node := heldNode(t, w, op)
		w.heldLease(op)
		w.heldLease(op, graph.UnitOf(node))
		now := time.Now().UTC()
		for _, p := range []types.ProjectID{graphProject, "prj_01m3250vkwffc979nzjw0g2sb2"} {
			a := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor, Name: "reader", CreatedBy: op}
			g := types.Grant{ID: types.GrantID(newID(t, "grt")), Actor: a.ID, Projects: types.ProjectScope{List: []types.ProjectID{p}},
				Actions: []types.ActionKind{}, Reads: types.GrantReads{Projects: types.GrantReadsProjects{Same: true}, Kinds: []types.Name{"hold"}},
				ValidFrom: types.Time(now.Add(-time.Hour).Format(time.RFC3339)), ExpiresAt: types.Time(now.Add(time.Hour).Format(time.RFC3339)),
				ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: op}
			w.do(op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				evs, err := identity.CreateActor(a)
				if err != nil {
					return nil, err
				}
				gr, err := authority.CreateGrant(tx, g)
				return append(evs, gr...), err
			})
			w.do(op, func(*eventstore.Tx) ([]eventstore.Event, error) {
				evs, s, err := identity.IssueToken(a.ID, op, now)
				readers[p] = s
				return evs, err
			})
		}
	})
	for p, want := range map[types.ProjectID]int{graphProject: 2, "prj_01m3250vkwffc979nzjw0g2sb2": 0} {
		code, out, errb := c.adc(readers[p].Reveal(), "holds", "list", "--json")
		var list struct{ Items []types.HoldRead }
		if code != entry.ExitOK || json.Unmarshal([]byte(out), &list) != nil || len(list.Items) != want {
			t.Fatalf("a reader of %s: %d %s %s", p, code, out, errb)
		}
		for _, h := range list.Items {
			if h.Project == nil || *h.Project != p {
				t.Errorf("a reader of %s read %+v", p, h)
			}
		}
	}
}
