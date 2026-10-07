// SPDX-License-Identifier: Apache-2.0

package authority

// Grants are events (04 "Grants"): an operator creates one for a live
// supervisor and may revoke it; a narrowing is a revocation and a new grant.
// GrantProjector keeps the latest record of each, from the events and the
// projections alone, so a rebuild folds to the same state. ReadScope is a
// reader's authority over the event stream, re-checked against these
// records (04 "Read authorization").

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// GrantEventSchema is the payload schema of every grant event.
const GrantEventSchema = "records/grant-event"

// GrantProjection holds each grant's latest event, keyed <actor>/<grant>,
// so an actor's grants are one prefix.
const GrantProjection = "grants"

var grantTransitions = []types.GrantEventTransition{types.GrantEventTransitionGrantCreated, types.GrantEventTransitionGrantRevoked}

// GrantKinds registers the grant event kinds with the event store.
func GrantKinds() eventstore.Kinds {
	k := eventstore.Kinds{}
	for _, t := range grantTransitions {
		k[string(t)+"/v1"] = eventstore.Kind{Schema: GrantEventSchema}
	}
	return k
}

func grantKey(actor types.ActorID, id types.GrantID) string { return string(actor) + "/" + string(id) }

func grantEvent(p types.GrantEvent) ([]eventstore.Event, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	id, err := ids.New("evt")
	if err != nil {
		return nil, err
	}
	return []eventstore.Event{{ID: id, Kind: string(p.Transition) + "/v1", Subject: string(p.Grant.ID), Payload: b}}, nil
}

// CreateGrant proposes a new grant.
func CreateGrant(g types.Grant) ([]eventstore.Event, error) {
	return grantEvent(types.GrantEvent{Transition: types.GrantEventTransitionGrantCreated, Grant: g})
}

// RevokeGrant proposes revoking actor's grant id at now.
func RevokeGrant(tx *eventstore.Tx, actor types.ActorID, id types.GrantID, now time.Time) ([]eventstore.Event, error) {
	cur, ok, err := loadJSON[types.GrantEvent](tx, GrantProjection, grantKey(actor, id))
	if err != nil || !ok {
		return nil, errors.Join(err, fmt.Errorf("authority: no grant %s for %s", id, actor))
	}
	at := types.Time(now.UTC().Format(time.RFC3339Nano))
	return grantEvent(types.GrantEvent{Transition: types.GrantEventTransitionGrantRevoked, Grant: cur.Grant, RevokedAt: &at})
}

// GrantsOf returns actor's grants and the ones among them revoked: what
// Decide's Request takes.
func GrantsOf(tx *eventstore.Tx, actor types.ActorID) ([]types.Grant, map[types.GrantID]bool, error) {
	keys, err := tx.Keys(GrantProjection, string(actor)+"/")
	if err != nil {
		return nil, nil, err
	}
	var grants []types.Grant
	revoked := map[types.GrantID]bool{}
	for _, k := range keys {
		e, ok, err := loadJSON[types.GrantEvent](tx, GrantProjection, k)
		// A record that vanished, is not this actor's, or is under another
		// key is damage: an error, never a grant read some other way.
		if err == nil && (!ok || e.Grant.Actor != actor || k != grantKey(actor, e.Grant.ID)) {
			err = fmt.Errorf("authority: grant record %s is damaged", k)
		}
		if err != nil {
			return nil, nil, err
		}
		grants = append(grants, e.Grant)
		// Anything but a created, never-revoked record counts as revoked.
		revoked[e.Grant.ID] = e.Transition != types.GrantEventTransitionGrantCreated || e.RevokedAt != nil
	}
	return grants, revoked, nil
}

// GrantProjector accepts grant events; other kinds pass through.
type GrantProjector struct{}

// Project is the eventstore.Hooks.Project fold for grant events. The
// event's actor is the command's: it must be a live operator, the grant's
// issuer at creation.
func (GrantProjector) Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		if !slices.ContainsFunc(grantTransitions, func(t types.GrantEventTransition) bool { return ev.Kind == string(t)+"/v1" }) {
			continue
		}
		var p types.GrantEvent
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return fmt.Errorf("authority: event %s: %w", ev.ID, err)
		}
		if err := applyGrant(tx, ev, p); err != nil {
			return fmt.Errorf("authority: event %s: grant %s: %w", ev.ID, p.Grant.ID, err)
		}
	}
	return nil
}

func applyGrant(tx *eventstore.Tx, ev eventstore.Event, p types.GrantEvent) error {
	g, key := p.Grant, grantKey(p.Grant.Actor, p.Grant.ID)
	if string(p.Transition)+"/v1" != ev.Kind || ev.Subject != string(g.ID) {
		return fmt.Errorf("kind %s, subject %s carry %s", ev.Kind, ev.Subject, p.Transition)
	}
	if by, err := liveActor(tx, types.ActorID(ev.Actor)); err != nil || by.Kind != types.ActorKindOperator {
		return errors.Join(err, errors.New("only a live operator creates or revokes a grant"))
	}
	cur, exists, err := loadJSON[types.GrantEvent](tx, GrantProjection, key)
	if err != nil {
		return err
	}
	switch p.Transition {
	case types.GrantEventTransitionGrantCreated:
		if exists || g.IssuedBy != types.ActorID(ev.Actor) {
			return errors.New("a grant is created once, by its issuer")
		}
		if a, err := liveActor(tx, g.Actor); err != nil || a.Kind != types.ActorKindSupervisor {
			return errors.Join(err, errors.New("the grantee is not a live supervisor"))
		}
		from, err1 := time.Parse(time.RFC3339Nano, string(g.ValidFrom))
		until, err2 := time.Parse(time.RFC3339Nano, string(g.ExpiresAt))
		if err1 != nil || err2 != nil || !from.Before(until) {
			return errors.New("its validity is not a time range")
		}
	case types.GrantEventTransitionGrantRevoked:
		if !exists || cur.Transition != types.GrantEventTransitionGrantCreated || cur.RevokedAt != nil || !sameGrant(cur.Grant, g) {
			return errors.New("a revocation names a live grant as it was issued")
		}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return tx.Put(GrantProjection, key, b)
}

func loadJSON[T any](tx *eventstore.Tx, projection, key string) (T, bool, error) {
	var v T
	raw, ok, err := tx.Get(projection, key)
	if err != nil || !ok {
		return v, false, err
	}
	return v, true, json.Unmarshal(raw, &v)
}

func liveActor(tx *eventstore.Tx, id types.ActorID) (types.Actor, error) {
	a, ok, err := loadJSON[types.Actor](tx, identity.ActorProjection, string(id))
	if err == nil && (!ok || a.RevokedAt != nil) {
		err = fmt.Errorf("actor %s is not live", id)
	}
	return a, err
}

// ReadScope is what a reader may read (04 "Read authorization"): everything
// for an operator or the system, else the union of the supervisor's grants
// in force when the scope was taken.
type ReadScope struct {
	actor  types.ActorID
	all    bool
	grants []types.Grant
}

// ReadScopeOf takes actor's read scope at now. Only an operator or the
// system reads everything; a supervisor reads under its grants in force;
// any other kind reads nothing. On an error the scope is empty.
func ReadScopeOf(tx *eventstore.Tx, actor types.Actor, now time.Time) (ReadScope, error) {
	s := ReadScope{actor: actor.ID}
	switch actor.Kind {
	case types.ActorKindOperator, types.ActorKindSystem:
		s.all = true
		return s, nil
	case types.ActorKindSupervisor:
	default:
		return s, nil
	}
	grants, revoked, err := GrantsOf(tx, actor.ID)
	if err != nil {
		return ReadScope{actor: actor.ID}, err
	}
	for _, g := range grants {
		if !revoked[g.ID] && inForce(g, now) {
			s.grants = append(s.grants, g)
		}
	}
	return s, nil
}

// Empty reports a scope that reads nothing.
func (s ReadScope) Empty() bool { return !s.all && len(s.grants) == 0 }

// Holds reports whether every grant the scope was taken under is still in
// force at now: a revoked or expired one ends the scope, whatever other
// grants remain (a narrowing is a revocation).
func (s ReadScope) Holds(tx *eventstore.Tx, now time.Time) (bool, error) {
	if s.all {
		return true, nil
	}
	if len(s.grants) == 0 {
		return false, nil // an empty scope holds nothing
	}
	grants, revoked, err := GrantsOf(tx, s.actor)
	if err != nil {
		return false, err
	}
	for _, held := range s.grants {
		i := slices.IndexFunc(grants, func(g types.Grant) bool { return g.ID == held.ID })
		if i < 0 || revoked[held.ID] || !inForce(held, now) || !sameGrant(grants[i], held) {
			return false, nil
		}
	}
	return true, nil
}

// sameGrant: a and b encode alike (a grant never changes but by revocation).
func sameGrant(a, b types.Grant) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// Reads reports whether the scope reads e, a record of e's domain (the
// kind's first segment: lease.granted/v1 is lease) in e's project.
func (s ReadScope) Reads(e eventstore.Event) bool {
	domain, _, _ := strings.Cut(e.Kind, ".")
	var project *types.ProjectID
	if e.Project != nil {
		p := types.ProjectID(*e.Project)
		project = &p
	}
	return s.ReadsRecord(types.Name(domain), project)
}

// ReadsRecord reports whether the scope reads a record of domain in
// project: some grant's read projects cover project (a record outside any
// project, nil, only under an every-project read) and its read kinds name
// domain. Operators and the system read everything.
func (s ReadScope) ReadsRecord(domain types.Name, project *types.ProjectID) bool {
	if s.all {
		return true
	}
	for _, r := range s.Reach() {
		if covers(types.Grant{Projects: r.Projects}, project) && slices.Contains(r.Kinds, domain) {
			return true
		}
	}
	return false
}

// ReadReach is what one grant reads: its read projects (same as its
// projects, resolved) and its read domains.
type ReadReach struct {
	Grant    types.GrantID
	Projects types.ProjectScope
	Kinds    []types.Name
}

// Everything reports a scope that reads everything (an operator or the system).
func (s ReadScope) Everything() bool { return s.all }

// Reach is the read reach of each grant the scope holds, in grant order.
func (s ReadScope) Reach() []ReadReach {
	out := make([]ReadReach, 0, len(s.grants))
	for _, g := range s.grants {
		projects := types.ProjectScope{List: g.Reads.Projects.List}
		if g.Reads.Projects.Same {
			projects = g.Projects
		}
		out = append(out, ReadReach{Grant: g.ID, Projects: projects, Kinds: g.Reads.Kinds})
	}
	return out
}
