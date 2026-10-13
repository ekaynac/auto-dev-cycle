// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"maps"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Class is how this build answers a registered action (14 D1).
type Class string

const (
	// Wired: a handler, and exactly one of a logic and an issuer.
	Wired Class = "wired"
	// Deferred: could_not_run/action_deferred, with nothing submitted.
	Deferred Class = "deferred"
	// Unregistered: a deprecated action, failed/action_unregistered.
	Unregistered Class = "unregistered"
)

// Owner is where a deferred action is owned: its milestone and the
// document that names it, or Unassigned where no document does.
type Owner struct{ Milestone, Ref string }

// Unassigned marks a deferred action no plan or design document assigns to
// a milestone yet; the operator assigns it.
const Unassigned = "unassigned"

// deferred are the registered actions this build does not wire, each
// deferred to its owning milestone (14 D1). An action is wired or listed
// here in the PR that registers it: Matrix refuses one that is neither.
var deferred = map[string]Owner{
	"project.create":                      {Milestone: "M1", Ref: "design/m1/00 §1 intake: handler built (P1), activated by P22a"},
	"profile.adopt":                       {Milestone: "M1", Ref: "design/m0/15 D1"},
	"task.create":                         {Milestone: "M1", Ref: "design/m1/00 §1 intake: handler built (P3), activated by P22a"},
	"review.request":                      {Milestone: "M1", Ref: "design/m0/15 D1"},
	"input.answer":                        {Milestone: "M1", Ref: "design/m1/00 §1 answers: handler built (P5), activated by P6"},
	"knowledge.change_protected_document": {Milestone: "M1", Ref: "plan/01 §4; plan/03 §4.6, §9"},
	"integration.change_topology":         {Milestone: "M1", Ref: "design/m0/15 D1"},
}

// wiredNotes are the wired actions whose handler, in this build, refuses
// some or all of what it is sent, and why.
var wiredNotes = map[string]string{
	"proposal.confirm": "every confirmation is refused: no source of review receipts (authority.NoReviewSource); human confirmation waits for WebAuthn (14 R6)",
	"agent.enrol":      "a supervisor's enrolment is refused operator_only until WebAuthn (14 H1 interim rule)",
}

// Row is one registered action's place in the matrix. Via is the request
// layer of a wired action ("logic" or "issuer"), Note what its handler
// refuses in this build; Owner is a deferred action's.
type Row struct {
	Action, Family string
	Class          Class
	Via, Note      string
	Owner          Owner
}

// Matrix classifies every action of reg, sorted by action. It refuses a
// current action that is neither wired nor deferred, one that is both, a
// handler without exactly one of a logic and an issuer, a logic or an
// issuer without a handler, and a deferred action reg does not register.
func Matrix(reg *vocab.Registry) ([]Row, error) {
	az := authorizer(reg)
	hs, ls, is := handlers(), logics(az), issuers(az)
	for action := range ls {
		if hs[action] == nil {
			return nil, fmt.Errorf("daemon: %s has a logic and no handler", action)
		}
	}
	for action := range is {
		if hs[action] == nil {
			return nil, fmt.Errorf("daemon: %s has an issuer and no handler", action)
		}
	}
	for action := range deferred {
		if _, ok := reg.Actions[action]; !ok {
			return nil, fmt.Errorf("daemon: deferred action %s is not registered", action)
		}
	}
	for action := range wiredNotes {
		if hs[action] == nil {
			return nil, fmt.Errorf("daemon: note on %s, which is not wired", action)
		}
	}
	var rows []Row
	for _, action := range slices.Sorted(maps.Keys(reg.Actions)) {
		a := reg.Actions[action]
		r := Row{Action: action, Family: string(a.Family)}
		_, wired := hs[action]
		owner, isDeferred := deferred[action]
		switch {
		case a.Deprecated != nil:
			r.Class = Unregistered
		case wired && isDeferred:
			return nil, fmt.Errorf("daemon: %s is both wired and deferred", action)
		case wired && (ls[action] == nil) == (is[action] == nil):
			return nil, fmt.Errorf("daemon: %s has a handler and not exactly one of a logic and an issuer", action)
		case wired && ls[action] != nil:
			r.Class, r.Via, r.Note = Wired, "logic", wiredNotes[action]
		case wired:
			r.Class, r.Via, r.Note = Wired, "issuer", wiredNotes[action]
		case isDeferred && owner.Milestone == "":
			return nil, fmt.Errorf("daemon: deferred %s has no milestone; write %s where none is named", action, Unassigned)
		case isDeferred:
			r.Class, r.Owner = Deferred, owner
		default:
			return nil, fmt.Errorf("daemon: %s is neither wired nor deferred; classify it (14 D1)", action)
		}
		rows = append(rows, r)
	}
	return rows, nil
}
