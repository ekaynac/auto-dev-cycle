// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"encoding/json"
	"net/http"
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
		if c["column"] != want[0] || c["approval"] != want[1] || c["admitted"] != false || c["project"] != prjA || c["title"] != "fix the parser" {
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

// F: a node of a task that is not admitted, or of no recorded task, has
// no column the records give: the read fails rather than place its card.
func TestBoardRefusesAnUnderivableCard(t *testing.T) {
	for name, task := range map[string]func(f *fixture) types.TaskID{
		"a Ready task's node": func(f *fixture) types.TaskID { return f.task(prjA, true).ID },
		"an unrecorded task":  func(f *fixture) types.TaskID { return types.TaskID(newID(f.t, "tsk")) },
	} {
		t.Run(name, func(t *testing.T) {
			f := open(t)
			f.project(f.op.ID, prjA)
			n := node(t, types.NodeKindBuild, types.NodeStateStateWaiting).Node
			id := task(f)
			f.must(f.op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
				return graph.Record(types.NodeRecorded{Task: id, Project: prjA, Node: n})
			})
			if status, b := read(t, f.registeredServer(nil), "/v1/board", f.opTok.Reveal()); status != http.StatusServiceUnavailable {
				t.Fatalf("the board: %d %s", status, b)
			}
		})
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
	n.State.State, n.Outcome = types.NodeStateStateCompleted, &outcome
	return n
}

func held(t *testing.T, n graph.NodeView) graph.NodeView {
	n.State.State = types.NodeStateStateHeld
	n.State.Holds = []types.NodeHold{{ID: types.HoldID(newID(t, "hld")), Hold: "source_invalid_config", SignalInstance: "cfg@2",
		Owner: types.OwnerOperator, Since: types.Time(since.Format(time.RFC3339Nano)), Pages: []types.NodeHoldPagesItem{}}}
	return n
}

func waitingOn(n graph.NodeView, name types.Name, src types.InputSpecSourceVariant) graph.NodeView {
	n.Node.Inputs = append(n.Node.Inputs, types.InputSpec{Name: name, Source: types.InputSpecSource{Variant: src}})
	n.State.State, n.State.WaitingOn = types.NodeStateStateWaiting, append(n.State.WaitingOn, name)
	return n
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
	for name, c := range map[string]struct {
		state    string
		approval *authority.TaskApproval
		nodes    []graph.NodeView
		column   types.BoardCardColumn
	}{
		"an unapproved spec":           {authority.TaskReady, nil, nil, types.BoardCardColumnAwaitingApproval},
		"a revoked approval":           {authority.TaskReady, approval(authority.ApprovalRevoked), nil, types.BoardCardColumnAwaitingApproval},
		"approved, not admitted":       {authority.TaskReady, approval(authority.ApprovalInForce), nil, types.BoardCardColumnReady},
		"admitted, nothing recorded":   {authority.TaskAdmitted, approval(authority.ApprovalInForce), nil, types.BoardCardColumnReady},
		"admitted, nothing running":    {authority.TaskAdmitted, nil, []graph.NodeView{node(t, types.NodeKindBuild, types.NodeStateStateRunnable)}, types.BoardCardColumnReady},
		"a running build":              {authority.TaskAdmitted, nil, []graph.NodeView{build}, types.BoardCardColumnRunning},
		"a held build":                 {authority.TaskAdmitted, nil, []graph.NodeView{held(t, build)}, types.BoardCardColumnRunning},
		"a gate claimed":               {authority.TaskAdmitted, nil, []graph.NodeView{completed(build, types.OutcomeEmpty), gate}, types.BoardCardColumnVerifying},
		"a review claimed":             {authority.TaskAdmitted, nil, []graph.NodeView{review}, types.BoardCardColumnVerifying},
		"a held review":                {authority.TaskAdmitted, nil, []graph.NodeView{held(t, review)}, types.BoardCardColumnVerifying},
		"review go, merge inactive":    {authority.TaskAdmitted, nil, done, types.BoardCardColumnDoneCandidate},
		"review no_go":                 {authority.TaskAdmitted, nil, []graph.NodeView{completed(review, types.OutcomeFailed)}, types.BoardCardColumnReady},
		"a superseded go review":       {authority.TaskAdmitted, nil, []graph.NodeView{superseded}, types.BoardCardColumnReady},
		"a repair build after a go":    {authority.TaskAdmitted, nil, append(slices.Clone(done), build), types.BoardCardColumnRunning},
		"a structural waiting(input)":  {authority.TaskAdmitted, nil, []graph.NodeView{waitingOn(gate, "candidate", &types.ResultSource{Type: "result", Producer: build.Node.ID})}, types.BoardCardColumnReady},
		"a review_requirement claimed": {authority.TaskAdmitted, nil, []graph.NodeView{node(t, types.NodeKindReviewRequirement, types.NodeStateStateClaimed)}, types.BoardCardColumnReady},
	} {
		tr := authority.TaskRecord{Task: types.Task{ID: types.TaskID(newID(t, "tsk")), Project: prjA, Spec: types.TaskCreateScope{Title: "t"}}, State: c.state, Approval: c.approval}
		card, err := api.BoardCard(tr, c.nodes, now)
		if err != nil || card.Column != c.column || card.Admitted != (c.state == authority.TaskAdmitted) || len(card.Nodes) != len(c.nodes) {
			t.Errorf("%s: %+v, %v; want %s", name, card, err, c.column)
		}
	}
}

// A held node's card names the hold, its signal, owner and age; a
// structural wait names its producer, or the owed input it answers.
func TestBoardBadges(t *testing.T) {
	build := node(t, types.NodeKindBuild, types.NodeStateStateClaimed)
	h := held(t, build)
	owed := types.OwedInputID(newID(t, "own"))
	gate := waitingOn(node(t, types.NodeKindGate, types.NodeStateStateWaiting), "candidate", &types.ResultSource{Type: "result", Producer: build.Node.ID})
	gate = waitingOn(gate, "answer", &types.AnswerSource{Type: "answer", OwedInput: owed, Revision: 1})
	tr := authority.TaskRecord{Task: types.Task{ID: types.TaskID(newID(t, "tsk")), Project: prjA}, State: authority.TaskAdmitted}
	card, err := api.BoardCard(tr, []graph.NodeView{h, gate}, since.Add(90*time.Second))
	if err != nil || len(card.Nodes) != 2 {
		t.Fatalf("%+v %v", card, err)
	}
	b := card.Nodes[0].Holds
	if len(b) != 1 || b[0].Hold != "source_invalid_config" || b[0].SignalInstance != "cfg@2" || b[0].Owner != types.OwnerOperator ||
		b[0].ID != h.State.Holds[0].ID || b[0].AgeSeconds != 90 || b[0].Since != h.State.Holds[0].Since {
		t.Errorf("the hold badge: %+v", b)
	}
	w := card.Nodes[1].Waiting
	if len(w) != 2 || w[0].Input != "candidate" || w[0].Producer == nil || *w[0].Producer != build.Node.ID || w[0].OwedInput != nil ||
		w[1].Producer != nil || w[1].OwedInput == nil || *w[1].OwedInput != owed {
		t.Errorf("the waiting badges: %+v", w)
	}
}

// F: a card whose column the records do not determine is never placed.
func TestBoardUnderivable(t *testing.T) {
	merge := node(t, types.NodeKindMerge, types.NodeStateStateRunnable)
	elsewhere := node(t, types.NodeKindBuild, types.NodeStateStateClaimed)
	elsewhere.Project = prjB
	undeclared := node(t, types.NodeKindGate, types.NodeStateStateWaiting)
	undeclared.State.WaitingOn = []types.Name{"candidate"}
	for name, c := range map[string]struct {
		state    string
		approval *authority.TaskApproval
		nodes    []graph.NodeView
	}{
		"an unknown task state":         {"cancelled", nil, nil},
		"a Ready task with a node":      {authority.TaskReady, nil, []graph.NodeView{node(t, types.NodeKindBuild, types.NodeStateStateClaimed)}},
		"an unknown approval status":    {authority.TaskReady, &authority.TaskApproval{Status: "pending"}, nil},
		"a kind outside M1(a)":          {authority.TaskAdmitted, nil, []graph.NodeView{merge}},
		"a node in another project":     {authority.TaskAdmitted, nil, []graph.NodeView{elsewhere}},
		"a wait on an undeclared input": {authority.TaskAdmitted, nil, []graph.NodeView{undeclared}},
	} {
		tr := authority.TaskRecord{Task: types.Task{ID: types.TaskID(newID(t, "tsk")), Project: prjA}, State: c.state, Approval: c.approval}
		if card, err := api.BoardCard(tr, c.nodes, since); err == nil || !strings.Contains(err.Error(), "not derivable") {
			t.Errorf("%s: placed in %q (%v)", name, card.Column, err)
		}
	}
}
