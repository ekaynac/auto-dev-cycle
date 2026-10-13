// SPDX-License-Identifier: Apache-2.0

package authority

// Projects (docs/design/m0/07; M1 slice plan §5.1 P1): project.create
// registers a project with its first profile revision. The scope is the
// profile itself, and the profile's id names the project.
//
// Decisions:
//   - The whole profile is validated by profile.Load (its schema, then its
//     semantic checks) in Derive, before authority judges anything and so
//     before any write: an invalid profile is ErrInvalidScope, with no event.
//   - The project is judged as authority judges any project: a supervisor
//     needs a grant in force naming project.create on the profile's id
//     (action_not_granted otherwise), and the seat. Operators are
//     unrestricted (D31). Only operators and supervisors create projects.
//   - The project's record is its expected state: --expect 0 is its
//     absence. A second create of the same id is stale, with or without an
//     expectation (checked by the logic, after authority).
//   - project.create is no Applier in M1, so a proposal of it could never be
//     applied: a create a grant's override would make a proposal is refused
//     (Derived.NotProposable, action_not_proposable).
//   - The event's payload is the profile as submitted; its fold re-runs
//     profile.Load, so a rebuild admits exactly what the command did.
//     Profile additions must therefore stay optional for stored events; a
//     profile that a stored one would fail needs project.created/v2.
//   - Inert until P22a: adcd projects project.created but does not wire
//     this handler; project.create stays deferred (the intake capability).

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	profiles "github.com/ekaynac/auto-dev-cycle/internal/profile"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The project action, its scope schema, its event kind and the projection
// holding the projects (ProjectRecord, by id).
const (
	ProjectCreateAction = "project.create"
	ProjectCreateScope  = "api/scope/project-create"
	ProjectCreated      = "project.created/v1"
	ProjectProjection   = "projects"
)

// projectNotProposable is why a project.create is never proposed.
const projectNotProposable = "project.create has no Applier in M1, so a proposal of it could never be applied"

// ProjectRecord is a registered project: its profile's adopted revision,
// that revision's content digest and the profile itself.
type ProjectRecord struct {
	ID            types.ProjectID `json:"id"`
	Name          string          `json:"name"`
	Revision      int64           `json:"revision"`
	ProfileDigest types.Digest    `json:"profile_digest"`
	Profile       json.RawMessage `json:"profile"`
	CreatedBy     types.ActorID   `json:"created_by"`
	CreatedAt     types.Time      `json:"created_at"`
}

// ProjectKinds registers the project event kind; its payload is the
// profile.
func ProjectKinds() eventstore.Kinds {
	return eventstore.Kinds{ProjectCreated: {Schema: profiles.Schema}}
}

// ProjectRecordOf is project id's record, which --expect is checked
// against.
func ProjectRecordOf(id types.ProjectID) func(*eventstore.Tx) (json.RawMessage, bool, error) {
	return func(tx *eventstore.Tx) (json.RawMessage, bool, error) { return tx.Get(ProjectProjection, id) }
}

// ProjectCreateHandler derives project.create.
type ProjectCreateHandler struct{}

// Derive loads the profile, then derives its project as the object, in
// that project.
func (ProjectCreateHandler) Derive(_ *eventstore.Tx, by types.Actor, input json.RawMessage) (Derived, error) {
	p, err := profileOf(input)
	if err != nil {
		return Derived{}, err
	}
	if by.Kind != types.ActorKindOperator && by.Kind != types.ActorKindSupervisor {
		return Derived{}, refusal(vocab.ReasonActorKindRefused, "actor %s is a %s; only operators and supervisors create projects", by.ID, by.Kind)
	}
	return Derived{Object: p.ID, Project: &p.ID, Record: ProjectRecordOf(p.ID), NotProposable: projectNotProposable}, nil
}

// profileOf is the scope's profile, refused as ErrInvalidScope unless the
// scope schema admits it and profile.Load passes it.
func profileOf(raw json.RawMessage) (*types.ProjectProfile, error) {
	err := contract.Validate(ProjectCreateScope, raw)
	var p *types.ProjectProfile
	if err == nil {
		p, err = profiles.Load(raw)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s input: %w", ErrInvalidScope, ProjectCreateAction, err)
	}
	return p, nil
}

// CreateProject is project.create's logic for c, a command the Authorizer
// allowed: the project under the bound id, with the profile as its first
// revision.
func CreateProject(c eventstore.Command) eventstore.Decide {
	return eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		id, bound := tx.Bound()
		if c.Action != ProjectCreateAction || !bound {
			return nil, nil, fmt.Errorf("authority: %s is no derived %s", c.Action, ProjectCreateAction)
		}
		p, err := profileOf(c.Input)
		if err == nil && p.ID != id {
			err = fmt.Errorf("authority: the bound object %s is not the profile's project %s", id, p.ID)
		}
		if err != nil {
			return nil, nil, err
		}
		// Checked here, after authority, so a caller who may not create the
		// project learns nothing of whether it exists.
		if _, exists, err := tx.Get(ProjectProjection, id); err != nil {
			return nil, nil, err
		} else if exists {
			return nil, nil, fmt.Errorf("authority: %w: project %s exists", eventstore.ErrStaleExpectedState, id)
		}
		evs, err := event(ProjectCreated, id, &p.ID, json.RawMessage(c.Input))
		if err != nil {
			return nil, nil, err
		}
		dg, err := eventstore.ContentDigest(c.Input)
		if err != nil {
			return nil, nil, err
		}
		res, err := json.Marshal(map[string]any{"project": id, "revision": 1, "profile_digest": dg})
		return evs, res, err
	})
}

// ProjectProjector admits project.created events into ProjectProjection;
// other kinds pass through. It reads only the events and the identity
// projection, so a rebuild reaches the same projects.
type ProjectProjector struct{}

// Project is the eventstore.Hooks.Project fold for the projects.
func (ProjectProjector) Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		if ev.Kind != ProjectCreated {
			continue
		}
		if err := addProject(tx, ev); err != nil {
			return fmt.Errorf("authority: event %s: project: %w", ev.ID, err)
		}
	}
	return nil
}

// addProject admits a new project whose profile passes profile.Load, under
// its event's subject and project, by a live operator or supervisor.
func addProject(tx *eventstore.Tx, ev eventstore.Event) error {
	p, err := profiles.Load(ev.Payload)
	if err != nil {
		return err
	}
	if ev.Subject != p.ID || ev.Project == nil || *ev.Project != p.ID {
		return errors.New("a project's event is under its id, in its project")
	}
	by, err := liveActor(tx, types.ActorID(ev.Actor))
	if err != nil || by.Kind != types.ActorKindOperator && by.Kind != types.ActorKindSupervisor {
		return errors.Join(err, errors.New("a live operator or supervisor creates a project"))
	}
	if _, exists, err := tx.Get(ProjectProjection, p.ID); err != nil || exists {
		return errors.Join(err, errors.New("the project exists"))
	}
	dg, err := eventstore.ContentDigest(ev.Payload)
	if err != nil {
		return err
	}
	rec, err := json.Marshal(ProjectRecord{ID: p.ID, Name: p.Name, Revision: 1, ProfileDigest: dg, Profile: ev.Payload,
		CreatedBy: types.ActorID(ev.Actor), CreatedAt: ev.At})
	if err != nil {
		return err
	}
	return tx.Put(ProjectProjection, p.ID, rec)
}
