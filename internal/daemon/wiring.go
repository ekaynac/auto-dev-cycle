// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/capability"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/recovery"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// ErrCommandsRefused is the writer's answer to every new command whose
// action is not wired: the Authorize hook rechecks the principal and asks
// the action's handler for its derivation, and an action without one fails
// closed (14 D1). (A repeat of a stored command, by the same live actor with
// the same key and input, still replays its stored result: the identity
// recheck, then the idempotency lookup, precede authorization, 03.)
var ErrCommandsRefused = authority.ErrActionNotWired

// hooks are the writer's hooks: the identity recheck, the authorizer over
// the wired handlers and the projector. proposal.confirm is wired without
// a source of review receipts, so every confirmation is refused
// (authority.NoReviewSource) until receipts are recorded.
func hooks(az authority.Authorizer, project func(*eventstore.Tx, []eventstore.Event) error) eventstore.Hooks {
	authorize := func(t *eventstore.Tx, c eventstore.Command) error {
		if slices.Contains(systemActions, c.Action) {
			return authorizeSystem(t, c)
		}
		return az.Authorize(t, c)
	}
	return eventstore.Hooks{Identify: identity.Recheck, Authorize: authorize, Project: project}
}

// systemActions are the controller's own commands, the system actor's
// alone: the bootstrap's, the upload purge, restart recovery's steps and
// its orphan refusal (P21a, P21b; #226 entry 81), and the model calls'
// intent, count and reconciliation (P12a, P11b, P20; #226 entry 60). None
// is a registered action a principal may ask for.
var systemActions = []string{bootstrapAction, issueAction, renewAction, recoverAction, api.PurgeAction,
	recovery.ActionLose, string(recovery.Revoke), string(recovery.Seal), string(recovery.Settle), recovery.ActionRefuse,
	modelcall.Action, modelcall.Counted, ledger.ReconcileAction}

// authorizer is the one authorizer over the wired handlers: the writer's
// Authorize hook and the request layer's logic (logics, issuers) share it.
func authorizer(reg *vocab.Registry) authority.Authorizer {
	return authority.Authorizer{Registry: reg, Handlers: handlers()}
}

// handlers are the wired actions' handlers: proposal.confirm,
// proposal.withdraw, note.add, the seat's, agent.enrol and actor.revoke
// (14 PR 7a), grant.create, grant.revoke and grant.extend (14 PR 7c, 7d),
// token.issue, token.revoke and token.replace (14 M4, PR 7b), and the
// gated actions of every active capability (gated). Every other
// registered action has none and is refused.
func handlers() map[string]authority.Handler {
	hs := authority.SeatHandlers(0)
	maps.Copy(hs, authority.AdminHandlers())
	maps.Copy(hs, authority.TokenHandlers())
	hs[authority.ConfirmAction] = authority.ConfirmHandler{}
	hs[authority.WithdrawAction] = authority.WithdrawHandler{}
	hs[authority.NoteAction] = authority.NoteHandler{}
	hs[authority.GrantCreateAction] = authority.GrantCreateHandler{}
	hs[authority.GrantRevokeAction] = authority.GrantRevokeHandler{}
	hs[authority.GrantExtendAction] = authority.GrantExtendHandler{}
	for action, h := range gated() {
		if c, _ := capability.Gating(action); c.Active() {
			hs[action] = h
		}
	}
	return hs
}

// gated are the handlers of the actions a capability wires
// (capability.Table): intake's project.create and task.create (P1, P3)
// and task.cancel (#226 entry 23), and answers' input.answer (P5, P6).
// Each is wired only while its capability is active, and deferred
// otherwise (deferredActions).
func gated() map[string]authority.Handler {
	return map[string]authority.Handler{authority.ProjectCreateAction: authority.ProjectCreateHandler{}, authority.TaskCreateAction: authority.TaskCreateHandler{},
		authority.TaskCancelAction: authority.TaskCancelHandler{}, authority.InputAnswerAction: authority.InputAnswerHandler{}}
}

// logic is a wired action's command logic, which the request layer passes
// to Submit with the command.
type logic func(eventstore.Command) eventstore.Decide

// issuer is the request layer of an action that issues a credential
// (14 N3): it mints outside the transaction and returns the logic to
// submit with the command and the delivery of what that commits.
type issuer func(context.Context, *eventstore.Store, eventstore.Command) (eventstore.Decide, func(eventstore.Committed) (identity.Secret, bool), error)

// request is the request layer of an action that acts again once its
// command is decided: the logic, and what runs on the commit.
type request func(eventstore.Command) (eventstore.Decide, func(eventstore.Committed) error)

// issuers are the credential-issuing actions' request layers: agent.enrol,
// token.issue and token.replace.
func issuers(az authority.Authorizer) map[string]issuer {
	return map[string]issuer{authority.EnrolAction: az.EnrolRequest, authority.TokenIssueAction: az.TokenIssueRequest,
		authority.TokenReplaceAction: az.TokenReplaceRequest}
}

// requests are the wired actions' request layers that act on the commit:
// task.create, which drops the uploads it published (P3).
func requests(az authority.Authorizer, uploads authority.Uploads) map[string]request {
	out := map[string]request{}
	if az.Handlers[authority.TaskCreateAction] != nil {
		out[authority.TaskCreateAction] = authority.TaskCreate{Uploads: uploads}.Request
	}
	return out
}

// logics are the wired actions' logic, by action: every action handlers
// wires has one, an issuer or a request (Matrix checks), and an action
// without any is refused before anything is submitted (14 D1).
func logics(az authority.Authorizer) map[string]logic {
	out := map[string]logic{authority.ConfirmAction: az.Confirm, authority.WithdrawAction: az.Withdraw, authority.NoteAction: authority.AddNote, authority.RevokeActorAction: az.RevokeActor,
		authority.GrantCreateAction: authority.GrantCreateHandler{}.Logic, authority.GrantRevokeAction: authority.GrantRevokeHandler{}.Logic,
		authority.GrantExtendAction: authority.GrantExtendHandler{}.Logic, authority.TokenRevokeAction: az.RevokeToken}
	for action, l := range map[string]logic{authority.ProjectCreateAction: authority.CreateProject, authority.TaskCancelAction: authority.CancelTask, authority.InputAnswerAction: authority.InputAnswer} {
		if az.Handlers[action] != nil {
			out[action] = l
		}
	}
	for action, h := range az.Handlers {
		if s, ok := h.(authority.SeatHandler); ok {
			out[action] = s.Logic
		}
	}
	return out
}

// effectKinds are the effect kinds of the active capabilities
// (capability.EffectKinds), each as the package implementing it defines
// it: model_call (P12a). An active kind no package defines refuses the
// start; any other kind stays unregistered, and IntendEffect refuses it
// (I15 effect).
func effectKinds() (eventstore.EffectKinds, error) {
	defs, out := modelcall.Effects(), eventstore.EffectKinds{}
	for _, k := range capability.EffectKinds() {
		d, ok := defs[k]
		if !ok {
			return nil, fmt.Errorf("daemon: the active effect kind %s has no definition in this build", k)
		}
		out[k] = d
	}
	return out, nil
}

// nodeKinds are the node kinds adcd records (I15 admission, Opus #298
// M2): M1(a)'s, and those the active capabilities add (merge, P22b). The
// graph fold refuses a node.recorded of any other, a template's or not,
// on append and on replay.
func nodeKinds() []types.NodeKind {
	ks := slices.Clone(graph.M1Kinds)
	for _, k := range capability.NodeKinds() {
		ks = append(ks, types.NodeKind(k))
	}
	return ks
}

// eventKinds merges the event kinds of every package the writer projects;
// a kind registered twice is refused rather than silently overwritten.
func eventKinds() (eventstore.Kinds, error) {
	all := eventstore.Kinds{}
	for _, ks := range []eventstore.Kinds{identity.Kinds(), authority.GrantKinds(), authority.SeatKinds(), authority.ProposalKinds(), authority.NoteKinds(), authority.ProjectKinds(), authority.TaskKinds(), authority.OwedKinds(), lease.Kinds(), ledger.Kinds(), modelcall.Kinds(), graph.Kinds(), api.BlobKinds()} {
		for k := range ks {
			if _, dup := all[k]; dup {
				return nil, fmt.Errorf("daemon: event kind %s is registered twice", k)
			}
		}
		maps.Copy(all, ks)
	}
	return all, nil
}

// projector is the writer's Project hook, composed as a rebuild replays
// it: each event goes through identity, then grants, the seat, proposals,
// notes, projects, tasks, owed inputs, leases with their capacity reservations, the usage
// ledger, the model calls' bindings, then the graph, before the next event (eventstore.ProjectEach).
// The graph comes after leases, whose projections its supersession fold
// reads. Units are nodes (graph.Units): an attempt starts only on a claimed
// node's unit, and a seal binds its node's project and task. A node is
// claimed only while its task is admitted, a build only under the task's
// approval in force (authority.Tasks; I22, I1 consumption). No wired
// action builds a graph event yet: admission's command is the reconcile
// loop's (P22a-4), so the graph's projections stay empty, and no attempt
// starts, until it runs.
func projector(blobs lease.Blobs, holds map[string]types.Hold, r capacity.Reservations) func(*eventstore.Tx, []eventstore.Event) error {
	leases := lease.Projector{Blobs: blobs, Holds: holds, Units: graph.Units{Tasks: authority.Tasks{}}} // an attempt starts only for an admitted task, its approval in force (Opus #309 M1)
	return eventstore.ProjectEach(
		identity.Projector{}.Project,
		authority.GrantProjector{}.Project,
		authority.SeatProjector{}.Project,
		authority.ProposalProjector{}.Project,
		authority.NoteProjector{}.Project,
		authority.ProjectProjector{}.Project,
		authority.TaskProjector{}.Project,
		authority.OwedProjector{}.Project,
		capacity.Hook(leases, r),
		ledger.Project,
		modelcall.Project, // each call's binding, by call and by lease: the settlement's (#226 entries 70, 78)
		graph.Projector{Answers: authority.Answers{}, Kinds: nodeKinds(), Tasks: authority.Tasks{}}.Project, // the folds check the recorded answers (M1 P6), the deferred surfaces (I15) and the claims' tasks (I22)
	)
}

// builtBeforeRecord are the projections a store started before the record
// of built projections existed (eventstore.BuildProjections) is taken to
// have built, whatever its record says. The list is frozen at #341's base
// (b29be8a): every projection projector folded there, lease_grants aside,
// which main folds from #328 on, so a store started before #328 lacks it.
// A projection added since (lease_grants, then #333's unit_open and
// graph_task_nodes, and every later one) goes into projections only: a
// store that never built it builds it from the log. Never extend this list.
var builtBeforeRecord = []string{
	identity.ActorProjection, identity.TokenProjection, identity.BootstrapProjection,
	authority.GrantProjection, authority.GrantIDProjection, authority.GrantLineageProjection, authority.GrantCommandProjection,
	authority.SeatProjection, authority.ProposalProjection, authority.PendingProjection, authority.NoteProjection,
	authority.ProjectProjection, authority.TaskProjection, authority.OwedProjection,
	lease.LeaseProjection, lease.AttemptProjection, lease.UnitProjection, lease.StopProjection,
	lease.TargetProjection, lease.RestoreProjection, lease.CycleProjection, lease.RefusalProjection,
	capacity.ReservedProjection, capacity.GrantedProjection, capacity.LiabilityProjection, capacity.LiableProjection,
	ledger.UsageProjection, ledger.MeasurementProjection, ledger.BindingProjection, ledger.LeaseBindingProjection,
	modelcall.FloorProjection,
	graph.NodeProjection, graph.ClaimProjection, graph.KeyProjection, graph.ReuseProjection, graph.RenewalProjection,
	graph.SuccessorProjection, graph.TaskProjection, graph.ResultProjection, graph.GenProjection, graph.HoldProjection,
	graph.RetryProjection, graph.EffectProjection, graph.ObservedProjection, graph.AttemptResultProjection,
	graph.AttemptProjection, graph.GenerationProjection, graph.RequirementProjection, graph.PreemptedProjection,
}

// addedSinceRecord are the projections projector folds that
// builtBeforeRecord does not hold, in the order they were added. A new
// projection is appended here, at version 1 in projections. Declare every
// projection name as a `const …Projection` string: TestProjectionsRegistered
// finds only those, and one it misses is compared, never built (Opus #342
// L2). A projection declared before its projector is wired is registered
// too, so a store records it as built while it is still empty; wiring it
// later over existing events is then refused, as on main, and that
// projection loses the build at start. Wire a projection's fold in the PR
// that declares it.
var addedSinceRecord = []string{
	lease.GrantProjection,                          // #328
	lease.OpenProjection, graph.TaskNodeProjection, // #333
}

// projections are the projections projector folds, each at its fold's
// version: Start builds from the log any of them a store has never built
// (#226 entry 107). They are builtBeforeRecord, then addedSinceRecord. One
// missing here is never built at start, only compared, as before.
var projections = func() []eventstore.Projection {
	var out []eventstore.Projection
	for _, name := range slices.Concat(builtBeforeRecord, addedSinceRecord) {
		out = append(out, eventstore.Projection{Name: name, Version: 1})
	}
	return out
}()
