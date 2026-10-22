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
//   - The event's payload is the profile as submitted, validated by
//     profile.Load at command time only. The fold reads just the id and
//     name, so a stored v1 event keeps replaying when the profile schema
//     or profile.Check later tighten (its payload schema stays
//     config/project-profile: additions to it must stay optional, or a
//     stricter profile needs project.created/v2).
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

// ProjectProfile is project id's adopted profile, as its record holds it;
// false: no such project. A record whose profile no longer loads, whose
// digest is not its profile's, or whose profile is another project's is an
// error, never a profile (launch fails closed on it; Opus #326 L9).
func ProjectProfile(tx *eventstore.Tx, id types.ProjectID) (*types.ProjectProfile, bool, error) {
	raw, ok, err := tx.Get(ProjectProjection, string(id))
	if err != nil || !ok {
		return nil, false, err
	}
	var rec ProjectRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, false, err
	}
	p, err := profiles.Load(rec.Profile)
	if err != nil {
		return nil, false, fmt.Errorf("authority: project %s's profile: %w", id, err)
	}
	if dg, err := eventstore.ContentDigest(rec.Profile); err != nil || dg != rec.ProfileDigest || p.ID != id {
		return nil, false, errors.Join(err, fmt.Errorf("authority: project %s's record does not hold its profile", id))
	}
	return p, true, nil
}

// ProjectDataPolicy is project id's data policy as its adopted profile
// records it: the bytes of the recorded profile's data_policy member, never
// a re-encoding of the decoded profile (Opus 22a-5c-3 r2 M1); false with no
// adopted profile.
func ProjectDataPolicy(tx *eventstore.Tx, id types.ProjectID) (json.RawMessage, bool, error) {
	if _, ok, err := ProjectProfile(tx, id); err != nil || !ok { // the record holds its profile
		return nil, ok, err
	}
	raw, _, err := tx.Get(ProjectProjection, string(id))
	var rec ProjectRecord
	var members map[string]json.RawMessage
	if err == nil {
		err = json.Unmarshal(raw, &rec)
	}
	if err == nil {
		err = json.Unmarshal(rec.Profile, &members)
	}
	dp, ok := members["data_policy"]
	if err == nil && !ok {
		err = fmt.Errorf("authority: project %s's profile holds no data policy", id)
	}
	return dp, true, err
}

// PolicyDigest is the digest of a data policy, policy its recorded bytes
// (ProjectDataPolicy): the SHA-256 of their RFC 8785 canonical form, never
// of a re-encoded struct, so a field the struct gains changes no stored
// policy's digest. A plan brief's restriction and a lease's data_floor
// check name a policy by it alike (Opus 22a-5c-3 r2 M1; Mythos L1).
func PolicyDigest(policy json.RawMessage) (types.Digest, error) {
	c, err := eventstore.Canonical(policy)
	if err != nil {
		return "", err
	}
	d, err := eventstore.ContentDigest(c)
	return types.Digest(d), err
}

// ProjectProfileDigest is the digest of project id's adopted profile, as
// its record holds it; false with none.
func ProjectProfileDigest(tx *eventstore.Tx, id types.ProjectID) (types.Digest, bool, error) {
	if _, ok, err := ProjectProfile(tx, id); err != nil || !ok { // the record holds its profile
		return "", ok, err
	}
	raw, _, err := tx.Get(ProjectProjection, string(id))
	var rec ProjectRecord
	if err == nil {
		err = json.Unmarshal(raw, &rec)
	}
	return rec.ProfileDigest, true, err
}

// ProjectCreateHandler derives project.create.
type ProjectCreateHandler struct{}

// Derive loads the profile, then derives its project as the object, in
// that project.
func (ProjectCreateHandler) Derive(_ *eventstore.Tx, by types.Actor, input json.RawMessage) (Derived, error) {
	if by.Kind != types.ActorKindOperator && by.Kind != types.ActorKindSupervisor {
		return Derived{}, refusal(vocab.ReasonActorKindRefused, "actor %s is a %s; only operators and supervisors create projects", by.ID, by.Kind)
	}
	p, err := profileOf(input)
	if err != nil {
		return Derived{}, err
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

// addProject admits a new project under its event's subject and project,
// by a live operator or supervisor. It never re-runs profile.Load: a
// profile accepted when its command ran stays accepted on replay.
func addProject(tx *eventstore.Tx, ev eventstore.Event) error {
	var p struct {
		ID   types.ProjectID `json:"id"`
		Name string          `json:"name"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	if p.ID == "" || ev.Subject != p.ID || ev.Project == nil || *ev.Project != p.ID {
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
