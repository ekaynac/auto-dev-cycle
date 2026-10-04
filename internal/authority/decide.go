// SPDX-License-Identifier: Apache-2.0

// Package authority is the only place that decides whether an action is
// allowed (docs/design/m0/04). It runs inside the writer transaction (03).
// Decide is the core: from the actor, the action, its computed change
// classes and the actor's grants it returns allow, propose (with the
// confirmation policy) or refuse. It never trusts an action's name for its
// sensitivity: the caller supplies the classes the effective change
// computation found.
package authority

import (
	"fmt"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Policy is a confirmation policy, ordered by strength.
type Policy int

const (
	None Policy = iota
	IndependentReview
	Human
)

func (p Policy) String() string {
	return [...]string{"none", "independent_review", "human"}[p]
}

func policyOf(s string) (Policy, bool) {
	switch s {
	case "independent_review":
		return IndependentReview, true
	case "human":
		return Human, true
	}
	return None, false
}

// Disposition is what happens to a command.
type Disposition int

const (
	Refuse Disposition = iota
	Allow
	Propose // record a proposal under Policy and wait for its confirmation
)

// Decision is authority's answer for one command.
type Decision struct {
	Disposition Disposition
	Policy      Policy         // for Propose
	Grant       *types.GrantID // the grant it acts under (supervisors)
	Classes     []string       // the change classes decided on, the action's own included
	Reason      vocab.Reason   // for Refuse
	Detail      string
}

// Request is one command as authority sees it.
type Request struct {
	Actor   types.Actor
	Action  string
	Project *types.ProjectID // nil for an action outside any project
	// Classes is the effective change's sensitive classes, computed by the
	// caller (the Authorize hook's classifier) from the current and resulting
	// state; empty when nothing sensitive changes. An action that is itself
	// a class carries that class regardless. Grantee must be set whenever the
	// change creates or extends a grant.
	Classes []string
	// Grants are the actor's grants; Revoked names the ones revoked since.
	Grants  []types.Grant
	Revoked map[types.GrantID]bool
	// Grantee is the actor a grant.create or grant.extend is for.
	Grantee *types.ActorID
	Now     time.Time
}

func refuse(reason vocab.Reason, format string, args ...any) Decision {
	return Decision{Disposition: Refuse, Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// protectedChanges are the two narrow actions that may change only their
// own protected paths (04 "Protected changes").
var protectedChanges = []string{"knowledge.change_protected_document", "integration.change_topology"}

// Decide applies note 04 to one command. Every unknown is a refusal.
func Decide(reg *vocab.Registry, r Request) Decision {
	if r.Actor.RevokedAt != nil {
		at, err := time.Parse(time.RFC3339Nano, string(*r.Actor.RevokedAt))
		if err != nil || !r.Now.Before(at) {
			return refuse(vocab.ReasonActorRevoked, "actor %s is revoked", r.Actor.ID)
		}
	}
	a, ok := reg.Actions[r.Action]
	if !ok || a.Deprecated != nil {
		return refuse(vocab.ReasonActionUnregistered, "action %q is not a registered, current action", r.Action)
	}
	// The policy is the strongest default among the change's classes; a
	// class without a default is refused, never read as none.
	// An action that is itself a change class (grant.create, agent.enrol,
	// …) always carries it, whatever the caller's computation found: its
	// sensitivity never depends on the effective change alone.
	classes := slices.Clone(r.Classes)
	if _, ok := reg.ChangeClasses[r.Action]; ok && !slices.Contains(classes, r.Action) {
		classes = append(classes, r.Action)
	}
	policy, everyActor := None, false
	for _, c := range classes {
		d, ok := reg.ChangeClasses[c]
		if !ok {
			return refuse(vocab.ReasonChangeClassUnknown, "change class %q has no confirmation default", c)
		}
		p, _ := policyOf(string(d.Confirmation))
		policy = max(policy, p)
		everyActor = everyActor || d.EveryActorKind
	}
	// A protected change (a document, the topology) can never widen
	// authority or data policy, for any actor and under any confirmation:
	// its effective change may hold no class but its own (04 "Protected
	// changes"). Anything more goes through the dedicated widening action.
	if slices.Contains(protectedChanges, r.Action) {
		for _, c := range classes {
			if c != r.Action {
				return refuse(vocab.ReasonProtectedChangeWidens, "%s also changes %s; use the dedicated action", r.Action, c)
			}
		}
	}
	switch r.Actor.Kind {
	case types.ActorKindOperator, types.ActorKindSystem:
		// Unrestricted (D31), except where a class binds every actor kind
		// (KA1: framework prose export needs a presence-verified human).
		if everyActor {
			return Decision{Disposition: Propose, Policy: Human, Classes: classes}
		}
		return Decision{Disposition: Allow, Classes: classes}
	case types.ActorKindSupervisor:
		return supervisor(r, classes, policy)
	}
	return refuse(vocab.ReasonActionNotGranted, "actor kind %q acts under no rule", r.Actor.Kind)
}

// supervisor: a grant in force must cover the action on the project; its
// override can only tighten the policy; a supervisor never creates or
// extends its own grant.
func supervisor(r Request, classes []string, policy Policy) Decision {
	// Keyed on the change, not the action's name: any command whose
	// effective change creates or extends a grant is a grant change.
	grantChange := slices.Contains(classes, "grant.create") || slices.Contains(classes, "grant.extend")
	if grantChange && (r.Grantee == nil || *r.Grantee == r.Actor.ID) {
		return refuse(vocab.ReasonSelfGrantRefused, "a supervisor never creates or extends its own grant")
	}
	var covering []types.Grant
	for _, g := range r.Grants {
		if g.Actor == r.Actor.ID && !r.Revoked[g.ID] && inForce(g, r.Now) && covers(g, r.Project) && slices.Contains(g.Actions, types.ActionKind(r.Action)) {
			covering = append(covering, g)
		}
	}
	if len(covering) == 0 {
		return refuse(vocab.ReasonActionNotGranted, "no grant in force covers %s for actor %s", r.Action, r.Actor.ID)
	}
	// Several grants in force: the strictest override wins (a narrower
	// policy always beats a wider one).
	// An override weaker than the change's default is refused, never
	// silently ignored (it would mean the grant was issued for a weaker
	// change than this one).
	slices.SortFunc(covering, func(a, b types.Grant) int { return compareIDs(a.ID, b.ID) })
	chosen, base := covering[0].ID, policy
	for _, g := range covering {
		o, ok := g.ConfirmationOverrides[r.Action]
		if !ok {
			continue
		}
		p, ok := policyOf(string(o))
		switch {
		case !ok || p < base:
			return refuse(vocab.ReasonGrantOverrideWeaker, "grant %s overrides %s with %s, weaker than the change's %s", g.ID, r.Action, o, base)
		case p > policy:
			policy, chosen = p, g.ID
		}
	}
	d := Decision{Disposition: Allow, Grant: &chosen, Classes: classes}
	if policy != None {
		d.Disposition, d.Policy = Propose, policy
	}
	return d
}

func inForce(g types.Grant, now time.Time) bool {
	from, err1 := time.Parse(time.RFC3339Nano, string(g.ValidFrom))
	until, err2 := time.Parse(time.RFC3339Nano, string(g.ExpiresAt))
	return err1 == nil && err2 == nil && !now.Before(from) && now.Before(until)
}

// covers: a grant for every project covers any project and actions outside
// one; a listed grant covers only its projects.
func covers(g types.Grant, project *types.ProjectID) bool {
	if g.Projects.All {
		return true
	}
	return project != nil && slices.Contains(g.Projects.List, *project)
}

func compareIDs(a, b types.GrantID) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
