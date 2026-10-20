// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
)

// The acceptance a task's card never shows.
const (
	hiddenName   = "tests/hidden_parser_test.go"
	hiddenDigest = "sha256:abababababababababababababababababababababababababababababababab"
	hiddenReq    = "the requirement text stays off the board"
)

// task commits the operator's task.created on project, with one hidden
// acceptance test, and its approval in the same command when approved.
func (f *fixture) task(project string, approved bool) types.Task {
	f.t.Helper()
	spec := types.TaskCreateScope{Project: types.ProjectID(project), Title: "fix the parser", Requirement: hiddenReq,
		Attachments: []types.Attachment{{Name: hiddenName, Digest: hiddenDigest}}}
	raw, err := json.Marshal(spec)
	if err != nil {
		f.t.Fatal(err)
	}
	dg, err := eventstore.ContentDigest(raw)
	if err != nil {
		f.t.Fatal(err)
	}
	var t types.Task
	f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		t = types.Task{ID: types.TaskID(newID(f.t, "tsk")), Project: spec.Project, Spec: spec, SpecDigest: dg, CreatedBy: f.op.ID,
			CreatedAt: types.Time(tx.Now().Format(time.RFC3339Nano)), Acceptance: []types.AcceptanceBlob{{Name: hiddenName, Digest: hiddenDigest,
				Class: types.AcceptanceBlobClassRestricted, WithheldFrom: []types.AcceptanceBlobWithheldFromItem{
					types.AcceptanceBlobWithheldFromItemBuilder, types.AcceptanceBlobWithheldFromItemReviewer}}}}
		payload, err := json.Marshal(t)
		evs := []eventstore.Event{{ID: newID(f.t, "evt"), Kind: authority.TaskCreated, Subject: t.ID, Project: &project, Payload: payload}}
		if approved {
			more, aerr := authority.ApprovalEvent(authority.TaskApprovalOf(t, raw, types.ApprovalID(newID(f.t, "apr")), t.CreatedAt))
			evs, err = append(evs, more...), aerr
		}
		return evs, err
	})
	return t
}

// admit writes task's record as admitted, as P22a's task.admitted fold
// will (no fold writes it before P22a).
func (f *fixture) admit(task types.TaskID) {
	f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		raw, _, err := tx.Get(authority.TaskProjection, task)
		var r authority.TaskRecord
		if err == nil {
			err = json.Unmarshal(raw, &r)
		}
		r.State = authority.TaskAdmitted
		b, merr := json.Marshal(r)
		if err == nil {
			err = merr
		}
		if err == nil {
			err = tx.Put(authority.TaskProjection, task, b)
		}
		return []eventstore.Event{{ID: newID(f.t, "evt"), Kind: "fixture.appended/v1", Subject: prjA, Payload: []byte(`{}`)}}, err
	})
}

// recordNode records a first-round node of kind for task in project.
func (f *fixture) recordNode(task types.TaskID, project string, kind types.NodeKind) types.NodeID {
	n := node(f.t, kind, types.NodeStateStateWaiting).Node
	f.must(f.op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Record(types.NodeRecorded{Task: task, Project: types.ProjectID(project), Node: n})
	})
	return n.ID
}

// The board over recorded tasks: an approved task is Ready, one without
// its approval Awaiting approval, neither admitted. A supervisor reading
// the board on A sees A's card only; no card carries the acceptance (its
// names, digests, the spec digest over them) or the requirement.
func TestBoardRead(t *testing.T) {
	f := open(t)
	f.grantOn([]string{prjA}, nil, "board")
	f.project(f.op.ID, prjA)
	f.project(f.op.ID, prjB)
	ready, awaiting, onB := f.task(prjA, true), f.task(prjA, false), f.task(prjB, true)
	srv := f.registeredServer(nil)
	all := items(t, srv, "board", f.opTok.Reveal())
	if want := sorted(string(ready.ID), string(awaiting.ID), string(onB.ID)); !slices.Equal(keysOf(all), want) {
		t.Fatalf("the operator's board: %v", keysOf(all))
	}
	for id, want := range map[types.TaskID][2]string{ready.ID: {"ready", "in_force"}, awaiting.ID: {"awaiting_approval", "none"}} {
		c := all[string(id)]
		if c["type"] != "placed" || c["column"] != want[0] || c["approval"] != want[1] || c["admitted"] != false || c["project"] != prjA ||
			c["title"] != "fix the parser" {
			t.Errorf("%s: %v", id, c)
		}
	}
	if sup := items(t, srv, "board", f.supTk.Reveal()); !slices.Equal(keysOf(sup), sorted(string(ready.ID), string(awaiting.ID))) {
		t.Errorf("the supervisor reading A's board read %v", keysOf(sup))
	}
	_, body := read(t, srv, "/v1/board", f.opTok.Reveal())
	for _, leak := range []string{hiddenName, hiddenDigest, string(ready.SpecDigest), hiddenReq, "attachments", "acceptance"} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("the board carries %s: %s", leak, body)
		}
	}
}

// F, around damage: A holds a good task, a Ready task with a node, a node
// of no recorded task, and a task record overwritten so it no longer
// reads as one (no node). A reader of B's board reads B's card as ever; a
// reader of A's reads A's good card and the three unplaced, with no
// column; the read stays 200.
func TestBoardAroundDamage(t *testing.T) {
	f := open(t)
	f.project(f.op.ID, prjA)
	f.project(f.op.ID, prjB)
	good, early, onB, broken := f.task(prjA, true), f.task(prjA, true), f.task(prjB, true), f.task(prjA, true)
	orphan := types.TaskID(newID(t, "tsk"))
	f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) { // a record that no longer reads as one, with no node
		err := tx.Put(authority.TaskProjection, broken.ID, json.RawMessage(`{"id":"`+string(broken.ID)+`","project":"`+prjA+`","state":5}`))
		return []eventstore.Event{{ID: newID(f.t, "evt"), Kind: "fixture.appended/v1", Subject: prjA, Payload: []byte(`{}`)}}, err
	})
	f.recordNode(early.ID, prjA, types.NodeKindBuild)
	f.recordNode(orphan, prjA, types.NodeKindBuild)
	_, readsB := f.another([]string{prjB}, "board")
	_, readsA := f.another([]string{prjA}, "board")
	srv := f.registeredServer(nil)
	if status, b := read(t, srv, "/v1/board", readsB); status != http.StatusOK {
		t.Fatalf("B's board: %d %s", status, b)
	}
	if b := items(t, srv, "board", readsB); !slices.Equal(keysOf(b), []string{string(onB.ID)}) || b[string(onB.ID)]["column"] != "ready" {
		t.Errorf("B's board: %v", b)
	}
	a := items(t, srv, "board", readsA)
	if !slices.Equal(keysOf(a), sorted(string(good.ID), string(early.ID), string(orphan), string(broken.ID))) || a[string(good.ID)]["column"] != "ready" {
		t.Fatalf("A's board: %v", a)
	}
	for id, reason := range map[types.TaskID]string{early.ID: "nodes_before_admission", orphan: "task_unrecorded", broken.ID: "damaged_record"} {
		u := a[string(id)]
		if _, placed := u["column"]; placed || u["type"] != "unplaced" || u["reason"] != reason || u["project"] != prjA {
			t.Errorf("%s: %v", id, u)
		}
	}
}

// An admitted task with a held build is Running, its badge's owner_ref
// sent only to a reader that also reads holds in the project.
func TestBoardOwnerRef(t *testing.T) {
	f := open(t)
	f.project(f.op.ID, prjA)
	task := f.task(prjA, true)
	f.admit(task.ID)
	build := f.recordNode(task.ID, prjA, types.NodeKindBuild)
	ref := "env-team"
	f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Raise(tx, build, types.NodeHold{ID: types.HoldID(newID(t, "hld")), Hold: "wrong_owner", SignalInstance: "fix@1",
			Owner: types.OwnerNamedOwner, OwnerRef: &ref, Pages: []types.NodeHoldPagesItem{}}, nil)
	})
	_, boardOnly := f.another([]string{prjA}, "board")
	_, withHolds := f.another([]string{prjA}, "board", "hold")
	holdsElsewhere, holdsOnB := f.another([]string{prjA}, "board")
	f.grantWith(func(g *types.Grant) { g.Actor = holdsElsewhere.ID }, []string{prjB}, nil, "hold")
	srv := f.registeredServer(nil)
	for bearer, want := range map[string]any{f.opTok.Reveal(): ref, withHolds: ref, boardOnly: nil, holdsOnB: nil} {
		c := items(t, srv, "board", bearer)[string(task.ID)]
		nodes, _ := c["nodes"].([]any)
		if c["column"] != "running" || c["admitted"] != true || len(nodes) != 1 {
			t.Fatalf("the admitted task: %v", c)
		}
		holds, _ := nodes[0].(map[string]any)["holds"].([]any)
		if len(holds) != 1 || holds[0].(map[string]any)["owner_ref"] != want || holds[0].(map[string]any)["hold"] != "wrong_owner" {
			t.Errorf("the badge: %v, want owner_ref %v", holds, want)
		}
	}
}

// A hold badge sends a strict subset of a hold read's fields, plus its
// age: never its reason, pages, holder or project.
func TestBoardHoldBadgeFields(t *testing.T) {
	props := func(path string, at ...string) []string {
		raw, err := os.ReadFile(path)
		var doc map[string]any
		if err == nil {
			err = json.Unmarshal(raw, &doc)
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range at {
			doc, _ = doc[k].(map[string]any)
		}
		p, _ := doc["properties"].(map[string]any)
		return slices.Sorted(maps.Keys(p))
	}
	badge := props("../../schemas/api/board.json", "$defs", "BoardHoldBadge")
	hold := props("../../schemas/api/hold.json")
	if want := []string{"age_seconds", "hold", "id", "owner", "owner_ref", "signal_instance", "since"}; !slices.Equal(badge, want) {
		t.Fatalf("the badge's fields: %v", badge)
	}
	shared := slices.DeleteFunc(slices.Clone(badge), func(k string) bool { return k == "age_seconds" })
	if len(shared) >= len(hold) || slices.ContainsFunc(shared, func(k string) bool { return !slices.Contains(hold, k) }) {
		t.Errorf("the badge's hold fields %v are no strict subset of %v", shared, hold)
	}
}

var since = time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)

// node is a first-round node of kind in state, with inputs.
func node(t *testing.T, kind types.NodeKind, state types.NodeStateState, inputs ...types.InputSpec) graph.NodeView {
	id := types.NodeID(newID(t, "nod"))
	return graph.NodeView{Node: types.Node{ID: id, Kind: kind, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`),
		Inputs: append([]types.InputSpec{}, inputs...), Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}},
		Project: prjA, State: types.NodeState{Node: id, State: state, WaitingOn: []types.Name{}, Holds: []types.NodeHold{}}}
}

func completed(n graph.NodeView, outcome types.Outcome) graph.NodeView {
	id := types.ResultID("res_01m3250vkwffc979nzjw0g2sf9")
	n.State.State, n.State.Result, n.Outcome = types.NodeStateStateCompleted, &id, &outcome
	return n
}

func round(n graph.NodeView, r int64) graph.NodeView {
	n.Node.Round = r
	return n
}

func held(t *testing.T, n graph.NodeView) graph.NodeView {
	n.State.State = types.NodeStateStateHeld
	ref := "env-team"
	n.State.Holds = []types.NodeHold{{ID: types.HoldID(newID(t, "hld")), Hold: "wrong_owner", SignalInstance: "fix@1",
		Owner: types.OwnerNamedOwner, OwnerRef: &ref, Since: types.Time(since.Format(time.RFC3339Nano)), Pages: []types.NodeHoldPagesItem{}}}
	return n
}

func waitingOn(n graph.NodeView, name types.Name, src types.InputSpecSourceVariant) graph.NodeView {
	n.Node.Inputs = append(n.Node.Inputs, types.InputSpec{Name: name, Source: types.InputSpecSource{Variant: src}})
	n.State.State, n.State.WaitingOn = types.NodeStateStateWaiting, append(n.State.WaitingOn, name)
	return n
}

func taskIn(t *testing.T, state string, approval *authority.TaskApproval) authority.TaskRecord {
	return authority.TaskRecord{Task: types.Task{ID: types.TaskID(newID(t, "tsk")), Project: prjA, Spec: types.TaskCreateScope{Title: "t"}},
		State: state, Approval: approval}
}

// The derivation examples of P4b's row, each from its records only.
func TestBoardColumns(t *testing.T) {
	now := since.Add(90 * time.Second)
	approval := func(status string) *authority.TaskApproval { return &authority.TaskApproval{Status: status} }
	build, gate, review := node(t, types.NodeKindBuild, types.NodeStateStateClaimed), node(t, types.NodeKindGate, types.NodeStateStateClaimed),
		node(t, types.NodeKindReview, types.NodeStateStateClaimed)
	done := []graph.NodeView{completed(build, types.OutcomeEmpty), completed(gate, types.OutcomeEmpty), completed(review, types.OutcomeEmpty)}
	superseded := completed(review, types.OutcomeEmpty)
	superseded.State.State = types.NodeStateStateSuperseded
	repaired := append(slices.Clone(done), completed(round(build, 2), types.OutcomeEmpty), round(node(t, types.NodeKindGate, types.NodeStateStateRunnable), 2))
	for name, c := range map[string]struct {
		state    string
		approval *authority.TaskApproval
		nodes    []graph.NodeView
		column   types.BoardPlacedCardColumn
	}{
		"an unapproved spec":                 {authority.TaskReady, nil, nil, types.BoardPlacedCardColumnAwaitingApproval},
		"a revoked approval":                 {authority.TaskReady, approval(authority.ApprovalRevoked), nil, types.BoardPlacedCardColumnAwaitingApproval},
		"approved, not admitted":             {authority.TaskReady, approval(authority.ApprovalInForce), nil, types.BoardPlacedCardColumnReady},
		"admitted, nothing recorded":         {authority.TaskAdmitted, approval(authority.ApprovalInForce), nil, types.BoardPlacedCardColumnReady},
		"admitted, nothing running":          {authority.TaskAdmitted, nil, []graph.NodeView{node(t, types.NodeKindBuild, types.NodeStateStateRunnable)}, types.BoardPlacedCardColumnReady},
		"a running build":                    {authority.TaskAdmitted, nil, []graph.NodeView{build}, types.BoardPlacedCardColumnRunning},
		"a held build":                       {authority.TaskAdmitted, nil, []graph.NodeView{held(t, build)}, types.BoardPlacedCardColumnRunning},
		"a gate claimed":                     {authority.TaskAdmitted, nil, []graph.NodeView{completed(build, types.OutcomeEmpty), gate}, types.BoardPlacedCardColumnVerifying},
		"a review claimed":                   {authority.TaskAdmitted, nil, []graph.NodeView{review}, types.BoardPlacedCardColumnVerifying},
		"a held review":                      {authority.TaskAdmitted, nil, []graph.NodeView{held(t, review)}, types.BoardPlacedCardColumnVerifying},
		"review go, merge inactive":          {authority.TaskAdmitted, nil, done, types.BoardPlacedCardColumnDoneCandidate},
		"review no_go":                       {authority.TaskAdmitted, nil, []graph.NodeView{completed(review, types.OutcomeFailed)}, types.BoardPlacedCardColumnReady},
		"a superseded go review":             {authority.TaskAdmitted, nil, []graph.NodeView{superseded}, types.BoardPlacedCardColumnReady},
		"a repair build after a go":          {authority.TaskAdmitted, nil, append(slices.Clone(done), round(build, 2)), types.BoardPlacedCardColumnRunning},
		"a repair completed, its gate ahead": {authority.TaskAdmitted, nil, repaired, types.BoardPlacedCardColumnReady},
		"the repair's own review go":         {authority.TaskAdmitted, nil, append(slices.Clone(repaired), completed(round(review, 2), types.OutcomeEmpty)), types.BoardPlacedCardColumnDoneCandidate},
		"a structural waiting(input)":        {authority.TaskAdmitted, nil, []graph.NodeView{waitingOn(gate, "candidate", &types.ResultSource{Type: "result", Producer: build.Node.ID})}, types.BoardPlacedCardColumnReady},
		"a review_requirement claimed":       {authority.TaskAdmitted, nil, []graph.NodeView{node(t, types.NodeKindReviewRequirement, types.NodeStateStateClaimed)}, types.BoardPlacedCardColumnReady},
		"a cancelled task":                   {authority.TaskCancelledState, approval(authority.ApprovalInForce), nil, types.BoardPlacedCardColumnDone},
	} {
		card, ok := api.BoardCard(taskIn(t, c.state, c.approval), c.nodes, now, true).Variant.(*types.BoardPlacedCard)
		if !ok || card.Column != c.column || card.Admitted != (c.state == authority.TaskAdmitted) || card.Cancelled != (c.state == authority.TaskCancelledState) ||
			len(card.Nodes) != len(c.nodes) {
			t.Errorf("%s: %+v; want %s", name, card, c.column)
		}
	}
}

// A held node's card names the hold, its signal, owner and age, and its
// owner_ref only when the reader reads holds; a structural wait names its
// producer, or the owed input it answers.
func TestBoardBadges(t *testing.T) {
	build := node(t, types.NodeKindBuild, types.NodeStateStateClaimed)
	h := held(t, build)
	owed := types.OwedInputID(newID(t, "own"))
	gate := waitingOn(node(t, types.NodeKindGate, types.NodeStateStateWaiting), "candidate", &types.ResultSource{Type: "result", Producer: build.Node.ID})
	gate = waitingOn(gate, "answer", &types.AnswerSource{Type: "answer", OwedInput: owed, Revision: 1})
	tr := taskIn(t, authority.TaskAdmitted, nil)
	for _, ownerRefs := range []bool{true, false} {
		card, ok := api.BoardCard(tr, []graph.NodeView{h, gate}, since.Add(90*time.Second), ownerRefs).Variant.(*types.BoardPlacedCard)
		if !ok || len(card.Nodes) != 2 {
			t.Fatalf("%+v", card)
		}
		b := card.Nodes[0].Holds
		if len(b) != 1 || b[0].Hold != "wrong_owner" || b[0].SignalInstance != "fix@1" || b[0].Owner != types.OwnerNamedOwner ||
			b[0].ID != h.State.Holds[0].ID || b[0].AgeSeconds != 90 || b[0].Since != h.State.Holds[0].Since || (b[0].OwnerRef != nil) != ownerRefs {
			t.Errorf("the hold badge (owner refs %t): %+v", ownerRefs, b)
		}
		w := card.Nodes[1].Waiting
		if len(w) != 2 || w[0].Input != "candidate" || w[0].Producer == nil || *w[0].Producer != build.Node.ID || w[0].OwedInput != nil ||
			w[1].Producer != nil || w[1].OwedInput == nil || *w[1].OwedInput != owed {
			t.Errorf("the waiting badges: %+v", w)
		}
	}
}

// F: a card whose column the records do not determine is never placed:
// it is unplaced, with its reason.
func TestBoardUnderivable(t *testing.T) {
	merge := node(t, types.NodeKindMerge, types.NodeStateStateRunnable)
	elsewhere := node(t, types.NodeKindBuild, types.NodeStateStateClaimed)
	elsewhere.Project = prjB
	undeclared := node(t, types.NodeKindGate, types.NodeStateStateWaiting)
	undeclared.State.WaitingOn = []types.Name{"candidate"}
	lost := completed(node(t, types.NodeKindReview, types.NodeStateStateClaimed), types.OutcomeEmpty)
	lost.Outcome = nil
	badSince := held(t, node(t, types.NodeKindBuild, types.NodeStateStateClaimed))
	badSince.State.Holds[0].Since = "yesterday"
	for name, c := range map[string]struct {
		state    string
		approval *authority.TaskApproval
		nodes    []graph.NodeView
		reason   types.BoardUnplacedCardReason
	}{
		"an unknown task state":         {"withdrawn", nil, nil, types.BoardUnplacedCardReasonUnknownState},
		"a cancelled task with a node":  {authority.TaskCancelledState, nil, []graph.NodeView{node(t, types.NodeKindBuild, types.NodeStateStateClaimed)}, types.BoardUnplacedCardReasonNodesBeforeAdmission},
		"a Ready task with a node":      {authority.TaskReady, nil, []graph.NodeView{node(t, types.NodeKindBuild, types.NodeStateStateClaimed)}, types.BoardUnplacedCardReasonNodesBeforeAdmission},
		"an unknown approval status":    {authority.TaskReady, &authority.TaskApproval{Status: "pending"}, nil, types.BoardUnplacedCardReasonUnknownState},
		"a kind outside M1(a)":          {authority.TaskAdmitted, nil, []graph.NodeView{merge}, types.BoardUnplacedCardReasonDeferredNodeKind},
		"a node in another project":     {authority.TaskAdmitted, nil, []graph.NodeView{elsewhere}, types.BoardUnplacedCardReasonNodeInAnotherProject},
		"a wait on an undeclared input": {authority.TaskAdmitted, nil, []graph.NodeView{undeclared}, types.BoardUnplacedCardReasonUndeclaredWait},
		"a selected result not held":    {authority.TaskAdmitted, nil, []graph.NodeView{lost}, types.BoardUnplacedCardReasonDamagedRecord},
		"a hold's since unreadable":     {authority.TaskAdmitted, nil, []graph.NodeView{badSince}, types.BoardUnplacedCardReasonDamagedRecord},
	} {
		tr := taskIn(t, c.state, c.approval)
		u, ok := api.BoardCard(tr, c.nodes, since, true).Variant.(*types.BoardUnplacedCard)
		if !ok || u.Reason != c.reason || u.ID != tr.ID || u.Project == nil || *u.Project != prjA {
			t.Errorf("%s: %+v, want unplaced %s", name, u, c.reason)
		}
	}
}
