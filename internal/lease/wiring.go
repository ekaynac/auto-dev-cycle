// SPDX-License-Identifier: Apache-2.0

package lease

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Projections: each lease and attempt with its revision, and each unit's
// current attempt.
const (
	LeaseProjection   = "leases"
	AttemptProjection = "attempts"
	UnitProjection    = "unit_attempts"
	StopProjection    = "attempt_stops"  // attempt id → the command that requested its stop
	StartProjection   = "attempt_starts" // attempt id → its start's time, RFC 3339 (adc task show's metrics, P22a-7)
	// FirstProjection: unit → its first started attempt, by its start
	// time, then its id (adc task show's first attempt, read by the task's
	// units, never by scanning every attempt: Astra #335 M1).
	FirstProjection = "unit_first_attempt"
	// GrantProjection: lease id → its grant's time, RFC 3339 (the USD day
	// rail, P17b); command/<command id> → the priced lease that command
	// granted (one per command, Astra #302 H2).
	GrantProjection = "lease_grants"
	// OpenProjection indexes, by unit, what still runs it: "<unit>/<id>" for
	// each lease that has not ended and each attempt that is not sealed
	// (Astra #316 M3: a unit's live work read by its prefix, never a scan).
	OpenProjection = "unit_open"
)

// GrantedAt is lease id's grant time; false for a lease never granted.
func GrantedAt(tx *eventstore.Tx, id types.LeaseID) (time.Time, bool, error) {
	raw, ok, err := tx.Get(GrantProjection, string(id))
	if err != nil || !ok {
		return time.Time{}, false, err
	}
	var at string
	if err := json.Unmarshal(raw, &at); err != nil {
		return time.Time{}, false, err
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	return t, err == nil, err
}

// StaleRecord refuses a command that expected another revision of a record.
type StaleRecord struct{ Detail string }

func (s *StaleRecord) Error() string {
	return "lease: " + vocab.ReasonStaleExpectedState.String() + ": " + s.Detail
}

type stored[T any] struct {
	Revision int64 `json:"revision"`
	Record   T     `json:"record"`
}

func load[T any](tx *eventstore.Tx, projection, id string) (T, int64, bool, error) {
	var s stored[T]
	raw, ok, err := tx.Get(projection, id)
	if err != nil || !ok {
		return s.Record, 0, false, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s.Record, 0, false, fmt.Errorf("lease: %s %s: %w", projection, id, err)
	}
	return s.Record, s.Revision, true, nil
}

func put[T any](tx *eventstore.Tx, projection, id string, revision int64, record T) error {
	raw, err := json.Marshal(stored[T]{revision, record})
	if err != nil {
		return err
	}
	switch r := any(record).(type) {
	case types.Lease:
		err = open(tx, r.Unit, string(r.ID), !Ended(r.State))
	case types.Attempt:
		err = open(tx, r.Unit, string(r.ID), !SealedStatus(r.Status))
	}
	return errors.Join(err, tx.Put(projection, id, raw))
}

// open indexes id under unit while running, and drops it once not.
func open(tx *eventstore.Tx, unit types.UnitID, id string, running bool) error {
	key := string(unit) + "/" + id
	if running {
		return tx.Put(OpenProjection, key, json.RawMessage(`true`))
	}
	return tx.Delete(OpenProjection, key)
}

// SealedStatus: an attempt in s publishes nothing more (sealed, completed or
// superseded).
func SealedStatus(s types.AttemptStatus) bool {
	return s == types.AttemptStatusSealed || s == types.AttemptStatusCompleted || s == types.AttemptStatusSuperseded
}

// Unended names a lease on unit that has not ended (settled or released),
// or an attempt on it that is not sealed, if any: what still runs a node,
// for its task's end (Astra #316 H1), with the open index's entry (the
// lease's or the attempt's id). It reads the unit's open index alone.
func Unended(tx *eventstore.Tx, unit types.UnitID) (what, entry string, running bool, err error) {
	key, ok, err := tx.FirstKey(OpenProjection, string(unit)+"/")
	if err != nil || !ok {
		return "", "", false, err
	}
	id := strings.TrimPrefix(key, string(unit)+"/")
	if strings.HasPrefix(id, "lea_") {
		l, _, ok, err := load[types.Lease](tx, LeaseProjection, id)
		if err == nil && (!ok || string(l.ID) != id || l.Unit != unit) { // the index's own record (Opus #432 r3)
			err = fmt.Errorf("lease: unit %s's open index names lease %s, of unit %q", unit, id, l.Unit)
		}
		return fmt.Sprintf("lease %s is %s", id, l.State), id, true, err
	}
	a, _, ok, err := load[types.Attempt](tx, AttemptProjection, id)
	if err == nil && (!ok || string(a.ID) != id || a.Unit != unit) {
		err = fmt.Errorf("lease: unit %s's open index names attempt %s, of unit %q", unit, id, a.Unit)
	}
	return fmt.Sprintf("attempt %s is %s", id, a.Status), id, true, err
}

func current[T any](tx *eventstore.Tx, projection, id string, expected int64) (T, error) {
	rec, rev, ok, err := load[T](tx, projection, id)
	switch {
	case err != nil:
		return rec, err
	case !ok:
		return rec, fmt.Errorf("lease: no %s record %s", projection, id)
	case rev != expected:
		return rec, &StaleRecord{fmt.Sprintf("%s %s is at revision %d, not %d", projection, id, rev, expected)}
	}
	return rec, nil
}

func unitAttempt(tx *eventstore.Tx, unit types.UnitID) (types.AttemptID, bool, error) {
	raw, ok, err := tx.Get(UnitProjection, string(unit))
	if err != nil || !ok {
		return "", false, err
	}
	var id types.AttemptID
	return id, true, json.Unmarshal(raw, &id)
}

type payload struct {
	Transition string                   `json:"transition"`
	Reason     *string                  `json:"reason"`
	Lease      *types.Lease             `json:"lease"`
	Attempt    *types.Attempt           `json:"attempt"`
	Successor  *types.Attempt           `json:"successor"`
	Result     *types.SubmittedResult   `json:"result"`
	Targets    *[]types.ResourceGroupID `json:"targets"` // lease.granted only
}

func newEvent(subject string, p payload) (eventstore.Event, error) {
	id, err := ids.New("evt")
	if err != nil {
		return eventstore.Event{}, err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return eventstore.Event{}, err
	}
	return eventstore.Event{ID: id, Kind: p.Transition + "/v1", Subject: subject, Payload: raw}, nil
}

func reasonOf(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// The command helpers below propose events; the projector, in the same
// writer transaction, is the one place that accepts or refuses them.

// RequestLease records a new lease. A waiting lease is re-requested only
// through ChangeLease, with its revision.
func RequestLease(tx *eventstore.Tx, l types.Lease) ([]eventstore.Event, error) {
	if _, _, ok, err := load[types.Lease](tx, LeaseProjection, string(l.ID)); err != nil || ok {
		return nil, errors.Join(err, fmt.Errorf("lease: lease %s already exists", l.ID))
	}
	e, err := newEvent(string(l.ID), payload{Transition: Requested, Lease: &l})
	return []eventstore.Event{e}, err
}

// ChangeLease applies one lease transition to the lease at the expected
// revision, deriving the record from the projection. A grant carries
// e.Targets, its evidence-target groups. It proposes a restore transition
// where the plain table has none; the projector accepts it only for a lease
// whose grant recorded a target.
func ChangeLease(tx *eventstore.Tx, id types.LeaseID, expected int64, e LeaseEvent, holds map[string]types.Hold) ([]eventstore.Event, error) {
	l, err := current[types.Lease](tx, LeaseProjection, string(id), expected)
	if err != nil {
		return nil, err
	}
	n, err := ApplyLease(l, e, false, holds)
	if errors.Is(err, ErrTransition) {
		n, err = ApplyLease(l, e, true, holds)
	}
	if err != nil {
		return nil, err
	}
	p := payload{Transition: e.Kind, Reason: reasonOf(e.Reason), Lease: &n}
	if e.Kind == Granted {
		t := append([]types.ResourceGroupID{}, e.Targets...)
		p.Targets = &t
	}
	ev, err := newEvent(string(id), p)
	return []eventstore.Event{ev}, err
}

// LeaseOf is the lease and its projection revision.
func LeaseOf(tx *eventstore.Tx, id types.LeaseID) (types.Lease, int64, bool, error) {
	return load[types.Lease](tx, LeaseProjection, string(id))
}

// AttemptAt is the attempt and its projection revision.
func AttemptAt(tx *eventstore.Tx, id types.AttemptID) (types.Attempt, int64, bool, error) {
	return load[types.Attempt](tx, AttemptProjection, string(id))
}

// Ended: a lease in s holds nothing any more (settled or released), so its
// unit may be placed again (P21: settlement before replacement).
func Ended(s types.LeaseState) bool {
	return s == types.LeaseStateSettled || s == types.LeaseStateReleased
}

// Replaceable refuses to replace an attempt whose lease old has not ended
// (settled or released): its settlement comes first (P21: X3's step 4
// before steps 5 and 6), and an active old lease is refused too, so a
// replacement never runs beside its predecessor's lease (#226 entry 83;
// I8's admission row). SupersedeAttempt checks it at admission; replay does
// not.
func Replaceable(old types.Lease) error {
	if !Ended(old.State) {
		return fmt.Errorf("lease: lease %s is %s; it settles before its attempt is replaced", old.ID, old.State)
	}
	return nil
}

// LiveOn is a lease on unit that is requested, waiting, granted or active,
// if any, from the unit's open index.
func LiveOn(tx *eventstore.Tx, unit types.UnitID) (types.LeaseID, bool, error) {
	keys, err := tx.Keys(OpenProjection, string(unit)+"/lea_")
	for _, k := range keys {
		l, _, _, lerr := load[types.Lease](tx, LeaseProjection, strings.TrimPrefix(k, string(unit)+"/"))
		if lerr != nil {
			return "", false, lerr
		}
		switch l.State {
		case types.LeaseStateRequested, types.LeaseStateWaiting, types.LeaseStateGranted, types.LeaseStateActive:
			return l.ID, true, nil
		}
	}
	return "", false, err
}

// RequestGranted requests l and grants it, holding no evidence target, in
// one command (lease.requested, then lease.granted): capacity reserves at
// the grant or refuses the command, so a refused grant records no lease.
func RequestGranted(tx *eventstore.Tx, l types.Lease) ([]eventstore.Event, error) {
	req, err := RequestLease(tx, l)
	if err != nil {
		return nil, err
	}
	g, err := ApplyLease(l, LeaseEvent{Kind: Granted}, false, nil)
	if err != nil {
		return nil, err
	}
	e, err := newEvent(string(l.ID), payload{Transition: Granted, Lease: &g, Targets: &[]types.ResourceGroupID{}})
	return append(req, e), err
}

// RefuseStart is the launch boundary's refusal of a granted lease before
// any attempt (lease.start_refused for reason), and its release, which
// returns its reservations, in one command.
func RefuseStart(tx *eventstore.Tx, id types.LeaseID, expected int64, reason string) ([]eventstore.Event, error) {
	l, err := current[types.Lease](tx, LeaseProjection, string(id), expected)
	if err != nil {
		return nil, err
	}
	var evs []eventstore.Event
	for _, e := range []LeaseEvent{{Kind: StartRefused, Reason: reason}, {Kind: Released}} {
		if l, err = ApplyLease(l, e, false, nil); err != nil {
			return nil, err
		}
		next := l
		ev, err := newEvent(string(id), payload{Transition: e.Kind, Reason: reasonOf(e.Reason), Lease: &next})
		if err != nil {
			return nil, err
		}
		evs = append(evs, ev)
	}
	return evs, nil
}

// StartNext starts the unit's next attempt under a granted lease: its
// first (token 1), or the successor of its sealed current attempt (the
// next token), as the unit's attempts say, never the caller.
func StartNext(tx *eventstore.Tx, leaseID types.LeaseID, expected int64, id types.AttemptID, base types.AttemptBase) ([]eventstore.Event, error) {
	l, err := current[types.Lease](tx, LeaseProjection, string(leaseID), expected)
	if err != nil {
		return nil, err
	}
	cur, ok, err := unitAttempt(tx, l.Unit)
	if err != nil {
		return nil, err
	}
	if !ok {
		return StartAttempt(tx, leaseID, expected, id, base)
	}
	_, rev, _, err := load[types.Attempt](tx, AttemptProjection, string(cur))
	if err != nil {
		return nil, err
	}
	return SupersedeAttempt(tx, cur, rev, leaseID, expected, id, base)
}

// RecordRestore records the outcome of a restore of a lease's evidence
// target (lease.restore_recorded), which lease.restore_verified then names.
func RecordRestore(tx *eventstore.Tx, r types.RestoreEvidence) ([]eventstore.Event, error) {
	b, err := json.Marshal(r)
	id, ierr := ids.New("evt")
	return []eventstore.Event{{ID: id, Kind: RestoreRecorded, Subject: string(r.Lease), Payload: b}}, errors.Join(err, ierr)
}

// StartAttempt starts a unit's first attempt (token 1) under a granted
// lease: one attempt.started event carries the attempt and the lease.
func StartAttempt(tx *eventstore.Tx, leaseID types.LeaseID, expected int64, id types.AttemptID, base types.AttemptBase) ([]eventstore.Event, error) {
	l, err := current[types.Lease](tx, LeaseProjection, string(leaseID), expected)
	if err != nil {
		return nil, err
	}
	a := Start(id, l.Unit, leaseID, base)
	active, err := ApplyLease(l, LeaseEvent{Kind: AttemptStarted, Attempt: &a.ID}, false, nil)
	if err != nil {
		return nil, err
	}
	e, err := newEvent(string(id), payload{Transition: AttemptStarted, Lease: &active, Attempt: &a})
	return []eventstore.Event{e}, err
}

// SupersedeAttempt retires the unit's sealed attempt and starts its
// replacement with the next token under a granted lease for the same unit,
// in one attempt.superseded event.
func SupersedeAttempt(tx *eventstore.Tx, old types.AttemptID, oldExpected int64, leaseID types.LeaseID, leaseExpected int64, id types.AttemptID, base types.AttemptBase) ([]eventstore.Event, error) {
	a, err := current[types.Attempt](tx, AttemptProjection, string(old), oldExpected)
	if err != nil {
		return nil, err
	}
	l, err := current[types.Lease](tx, LeaseProjection, string(leaseID), leaseExpected)
	if err != nil {
		return nil, err
	}
	if was, _, ok, err := LeaseOf(tx, a.Lease); err != nil || !ok || Replaceable(was) != nil {
		return nil, errors.Join(err, Replaceable(was))
	}
	retired, next, err := Supersede(a, id, leaseID, base)
	if err != nil {
		return nil, err
	}
	active, err := ApplyLease(l, LeaseEvent{Kind: AttemptStarted, Attempt: &next.ID}, false, nil)
	if err != nil {
		return nil, err
	}
	e, err := newEvent(string(old), payload{Transition: Superseded, Lease: &active, Attempt: &retired, Successor: &next})
	return []eventstore.Event{e}, err
}

// ChangeAttempt applies one attempt transition to the attempt at the
// expected revision. A result submission goes through SubmitResult, which
// carries the result; this helper proposes one without it, which the
// projector refuses.
func ChangeAttempt(tx *eventstore.Tx, id types.AttemptID, expected int64, e AttemptEvent) ([]eventstore.Event, error) {
	a, err := current[types.Attempt](tx, AttemptProjection, string(id), expected)
	if err != nil {
		return nil, err
	}
	n, err := ApplyAttempt(a, e)
	if err != nil {
		return nil, err
	}
	ev, err := newEvent(string(id), payload{Transition: e.Kind, Attempt: &n})
	return []eventstore.Event{ev}, err
}

// RecoverAfterRestart is the first step after a controller restart: every
// unit's running attempt becomes lost/controller_restart (publication
// closed) before anything else runs (05). The startup integration calls it
// in its own command; replay never synthesizes it.
func RecoverAfterRestart(tx *eventstore.Tx) ([]eventstore.Event, error) {
	units, err := tx.Keys(UnitProjection, "")
	if err != nil {
		return nil, err
	}
	var evs []eventstore.Event
	for _, u := range units {
		id, _, err := unitAttempt(tx, types.UnitID(u))
		if err != nil {
			return nil, err
		}
		a, _, ok, err := load[types.Attempt](tx, AttemptProjection, string(id))
		if err != nil || !ok {
			return nil, errors.Join(err, fmt.Errorf("lease: unit %s's attempt %s is missing", u, id))
		}
		if a.Status != types.AttemptStatusRunning {
			continue
		}
		lost := LoseOnRestart(a)
		e, err := newEvent(string(id), payload{Transition: Lost, Attempt: &lost})
		if err != nil {
			return nil, err
		}
		evs = append(evs, e)
	}
	return evs, nil
}

// FenceOutput fences an output against the unit's current attempt (05).
func FenceOutput(tx *eventstore.Tx, unit types.UnitID, id types.AttemptID, token int64) error {
	cur, ok, err := unitAttempt(tx, unit)
	if err != nil {
		return err
	}
	if !ok {
		return &Stale{fmt.Sprintf("unit %s has no attempt", unit)}
	}
	a, _, ok, err := load[types.Attempt](tx, AttemptProjection, string(cur))
	if err != nil || !ok {
		return errors.Join(err, fmt.Errorf("lease: unit %s's attempt %s is missing", unit, cur))
	}
	return Fence(a, id, token)
}

// Units binds a unit to its node (M1: a unit is a node, graph.Units): the
// project and task a seal's AttemptRecord must match, and whether an
// attempt may start on it. Without it, no attempt starts and no seal is
// admitted.
type Units interface {
	ProjectTask(tx *eventstore.Tx, unit types.UnitID) (types.ProjectID, types.TaskID, bool, error)
	// Startable refuses an attempt starting on unit now.
	Startable(tx *eventstore.Tx, unit types.UnitID) error
}

// Blobs is what the projector reads to check a seal's AttemptRecord.
type Blobs interface {
	Has(digest string) (bool, error)
	Open(digest string) (io.ReadCloser, error)
}

// Projector folds lease and attempt events into the projections, and is the
// one place that accepts them. Each event is checked on its own, so a rebuild
// replays it the same way: its kind is its payload's transition and its
// subject its record; every record it carries is exactly what the state
// machines derive from the projected one; an attempt starts only on a unit
// its binding admits (Units.Startable); a first attempt is the unit's
// token-1 attempt under its lease, a successor the next token in the same
// event as its predecessor's retirement; a lease leaves active only once its
// attempt stopped publishing (completed: once it completed); a seal names a
// valid AttemptRecord of its attempt.
//
// Admission checks that read state outside the log (the checkpoint blob and
// its named blobs, the unit's project and task, the hold registry) run only
// when an event is admitted, never on replay (Tx.Replaying), so purged blobs
// or a retired hold never make recorded history unrebuildable; the fold
// itself is deterministic from the log.
type Projector struct {
	Blobs  Blobs
	Holds  map[string]types.Hold
	Units  Units
	Groups Groups // optional: without it, no lease reserving slots settles
}

// Groups says, from projections the log derives, whether a lease is known
// to hold no evidence target, so it owes no restore (internal/capacity).
type Groups interface {
	RestoreFree(tx *eventstore.Tx, id types.LeaseID) (bool, error)
}

// Project is the event store's Project hook.
func (p Projector) Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		if ev.Kind == RestoreRecorded {
			if err := restoreRecorded(tx, ev); err != nil {
				return fmt.Errorf("lease: event %s: %w", ev.ID, err)
			}
			continue
		}
		if ev.Kind == OutputRefused {
			if err := refused(tx, ev); err != nil {
				return fmt.Errorf("lease: event %s: %w", ev.ID, err)
			}
			continue
		}
		if !isLeaseKind(ev.Kind) {
			continue
		}
		var pl payload
		if err := json.Unmarshal(ev.Payload, &pl); err != nil {
			return fmt.Errorf("lease: event %s: %w", ev.ID, err)
		}
		if err := p.apply(tx, ev, pl); err != nil {
			return fmt.Errorf("lease: event %s: %w", ev.ID, err)
		}
	}
	return nil
}

func isLeaseKind(kind string) bool {
	for _, t := range transitions {
		if kind == t+"/v1" {
			return true
		}
	}
	return false
}

func (p Projector) apply(tx *eventstore.Tx, ev eventstore.Event, pl payload) error {
	if ev.Kind != pl.Transition+"/v1" {
		return fmt.Errorf("a %s carrying %s", ev.Kind, pl.Transition)
	}
	subject := ""
	if pl.Attempt != nil {
		subject = string(pl.Attempt.ID)
	} else if pl.Lease != nil {
		subject = string(pl.Lease.ID)
	}
	if ev.Subject != subject {
		return fmt.Errorf("subject %s is not its record %s", ev.Subject, subject)
	}
	switch pl.Transition {
	case ResultSubmitted:
		if pl.Result == nil {
			return errors.New("a result submission without its result")
		}
		return p.changeAttempt(tx, pl.Transition, *pl.Attempt)
	case AttemptStarted:
		return errors.Join(started(tx, ev, *pl.Attempt), p.start(tx, *pl.Lease, *pl.Attempt, nil))
	case Superseded:
		return errors.Join(started(tx, ev, *pl.Successor), p.start(tx, *pl.Lease, *pl.Successor, pl.Attempt))
	case StopRequested:
		cmd, err := json.Marshal(ev.Command)
		if err == nil {
			err = tx.Put(StopProjection, string(pl.Attempt.ID), cmd)
		}
		return errors.Join(err, p.changeAttempt(tx, pl.Transition, *pl.Attempt))
	case Lost, Sealed, AttemptDone:
		return p.changeAttempt(tx, pl.Transition, *pl.Attempt)
	}
	if pl.Lease == nil {
		return fmt.Errorf("a %s carries no lease", pl.Transition)
	}
	if pl.Targets != nil { // only a grant's; records/lease-event refuses one elsewhere
		if err := granted(tx, *pl.Lease, *pl.Targets); err != nil {
			return err
		}
	}
	if pl.Transition == Granted {
		if err := p.grantedOnce(tx, ev, *pl.Lease); err != nil {
			return err
		}
		at, err := json.Marshal(ev.At)
		if err == nil {
			err = tx.Put(GrantProjection, string(pl.Lease.ID), at)
		}
		if err != nil {
			return err
		}
	}
	return p.changeLease(tx, pl.Transition, pl.Reason, *pl.Lease)
}

// grantedOnce refuses a second priced lease granted by ev's command: each
// placement prices its lease against the spend committed before its
// command (dispatch.Match), so two in one command could each fit a USD
// rail they pass together (Astra #302 H2). One per command keeps every
// placement's check sound, for the task's rail and the day's.
func (p Projector) grantedOnce(tx *eventstore.Tx, ev eventstore.Event, l types.Lease) error {
	if l.Price == nil || ev.Command == nil {
		return nil
	}
	key := "command/" + *ev.Command
	if other, dup, err := tx.Get(GrantProjection, key); err != nil || dup {
		return errors.Join(err, fmt.Errorf("command %s grants priced lease %s after %s: one per command", *ev.Command, l.ID, other))
	}
	id, err := json.Marshal(l.ID)
	if err != nil {
		return err
	}
	return tx.Put(GrantProjection, key, id)
}

// started records attempt's start time, ev's, and keeps its unit's first
// attempt.
func started(tx *eventstore.Tx, ev eventstore.Event, attempt types.Attempt) error {
	at, err := json.Marshal(ev.At)
	if err == nil {
		err = tx.Put(StartProjection, string(attempt.ID), at)
	}
	if err != nil {
		return err
	}
	return first(tx, attempt.Unit, attempt.ID)
}

// first makes attempt unit's first attempt unless the one recorded
// started earlier, or at the same time with a smaller id.
func first(tx *eventstore.Tx, unit types.UnitID, attempt types.AttemptID) error {
	at, _, err := StartedAt(tx, attempt)
	if err != nil {
		return err
	}
	if cur, ok, err := FirstOn(tx, unit); err != nil {
		return err
	} else if ok {
		was, _, err := StartedAt(tx, cur)
		if err != nil || was.Before(at) || was.Equal(at) && cur < attempt {
			return err
		}
	}
	id, err := json.Marshal(attempt)
	if err == nil {
		err = tx.Put(FirstProjection, string(unit), id)
	}
	return err
}

// FirstOn is unit's first started attempt (by its start time, then its
// id); false before one starts.
func FirstOn(tx *eventstore.Tx, unit types.UnitID) (types.AttemptID, bool, error) {
	raw, ok, err := tx.Get(FirstProjection, string(unit))
	var id types.AttemptID
	if err == nil && ok {
		err = json.Unmarshal(raw, &id)
	}
	return id, ok && err == nil, err
}

// StartedAt is attempt id's start time; false for an attempt never started.
func StartedAt(tx *eventstore.Tx, id types.AttemptID) (time.Time, bool, error) {
	raw, ok, err := tx.Get(StartProjection, string(id))
	var at string
	if err == nil && ok {
		err = json.Unmarshal(raw, &at)
	}
	if err != nil || !ok {
		return time.Time{}, false, err
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	return t, err == nil, err
}

// same compares two records by their canonical JSON.
func same(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

func (p Projector) changeLease(tx *eventstore.Tx, transition string, reason *string, n types.Lease) error {
	prev, rev, ok, err := load[types.Lease](tx, LeaseProjection, string(n.ID))
	if err != nil {
		return err
	}
	if !ok {
		fresh := n
		fresh.State, fresh.Hold, fresh.Attempt, fresh.RestoreReceipt, fresh.Settlement = types.LeaseStateRequested, nil, nil, nil, nil
		if transition != Requested || !same(fresh, n) {
			return fmt.Errorf("lease %s does not exist", n.ID)
		}
		return put(tx, LeaseProjection, string(n.ID), 1, n)
	}
	e := LeaseEvent{Kind: transition, Hold: n.Hold, Receipt: n.RestoreReceipt, Settlement: n.Settlement}
	if reason != nil {
		e.Reason = *reason
	}
	holds := p.Holds
	if tx.Replaying() && n.Hold != nil {
		// The hold was registered when admitted; replay does not re-ask a
		// registry that may have changed since.
		holds = map[string]types.Hold{n.Hold.Hold: {}}
	}
	// Evidence targets count slots (12, 13), so a lease with a slot
	// reservation may owe a restore: it settles plainly only once Groups
	// says it holds no evidence target.
	targets, err := targetsOf(tx, n.ID)
	if err != nil {
		return err
	}
	target := len(targets) > 0
	if transition == RestoreVerified {
		if err := verified(tx, n, targets); err != nil {
			return err
		}
	}
	if transition == Settled && !target {
		if err := p.restoreFree(tx, prev); err != nil {
			return err
		}
	}
	want, err := ApplyLease(prev, e, target, holds)
	if err != nil {
		return err
	}
	if !same(want, n) {
		return fmt.Errorf("lease %s is not what %s derives", n.ID, transition)
	}
	if prev.State == types.LeaseStateActive && prev.Attempt != nil {
		a, _, ok, err := load[types.Attempt](tx, AttemptProjection, string(*prev.Attempt))
		switch {
		case err != nil:
			return err
		case !ok:
			return fmt.Errorf("lease %s's attempt is missing", n.ID)
		case transition == Completed && a.Status != types.AttemptStatusCompleted:
			return fmt.Errorf("lease %s completes before its attempt (%s)", n.ID, a.Status)
		case a.Publication == types.AttemptPublicationOpen:
			return fmt.Errorf("lease %s leaves active while its attempt still publishes; stop it first", n.ID)
		}
	}
	if target {
		if err := restoreCycle(tx, transition, n.ID); err != nil {
			return err
		}
	}
	return put(tx, LeaseProjection, string(n.ID), rev+1, n)
}

// restoreFree refuses a plain settlement of a lease with a slot
// reservation unless Groups knows it holds no evidence target.
func (p Projector) restoreFree(tx *eventstore.Tx, l types.Lease) error {
	if !slices.ContainsFunc(l.Reservations, func(r types.LeaseReservationsItem) bool { return r.Unit == types.CapacityUnitSlots }) {
		return nil
	}
	free := false
	if p.Groups != nil {
		var err error
		if free, err = p.Groups.RestoreFree(tx, l.ID); err != nil {
			return err
		}
	}
	if !free {
		return fmt.Errorf("lease %s reserves slots, which may owe a restore; it settles once groups are known", l.ID)
	}
	return nil
}

// start accepts a first attempt (retired nil) or a successor with its
// predecessor's retirement, and the lease it runs under.
func (p Projector) start(tx *eventstore.Tx, l types.Lease, next types.Attempt, retired *types.Attempt) error {
	prevLease, leaseRev, ok, err := load[types.Lease](tx, LeaseProjection, string(l.ID))
	if err != nil || !ok {
		return errors.Join(err, fmt.Errorf("lease %s does not exist", l.ID))
	}
	active, err := ApplyLease(prevLease, LeaseEvent{Kind: AttemptStarted, Attempt: &next.ID}, false, nil)
	if err != nil || !same(active, l) {
		return errors.Join(err, fmt.Errorf("lease %s is not what attempt.started derives", l.ID))
	}
	if _, _, exists, err := load[types.Attempt](tx, AttemptProjection, string(next.ID)); err != nil || exists {
		return errors.Join(err, fmt.Errorf("attempt %s already exists", next.ID))
	}
	// The binding is read from projections the log derives (graph.Units),
	// so replay checks it again, at the same point of the log.
	if p.Units == nil {
		return fmt.Errorf("attempt %s: no unit binding, so no attempt starts", next.ID)
	}
	if err := p.Units.Startable(tx, l.Unit); err != nil {
		return err
	}
	cur, hasCur, err := unitAttempt(tx, l.Unit)
	if err != nil {
		return err
	}
	if retired == nil {
		if hasCur || !same(Start(next.ID, l.Unit, l.ID, next.Base), next) {
			return fmt.Errorf("attempt %s is not unit %s's first, token-1 attempt", next.ID, l.Unit)
		}
	} else {
		prev, rev, ok, err := load[types.Attempt](tx, AttemptProjection, string(retired.ID))
		if err != nil || !ok || !hasCur || cur != retired.ID {
			return errors.Join(err, fmt.Errorf("attempt %s is not unit %s's current attempt", retired.ID, l.Unit))
		}
		wantOld, wantNext, err := Supersede(prev, next.ID, l.ID, next.Base)
		if err != nil || !same(wantOld, *retired) || !same(wantNext, next) {
			return errors.Join(err, fmt.Errorf("supersession of %s is not what the state machine derives", retired.ID))
		}
		if err := put(tx, AttemptProjection, string(retired.ID), rev+1, *retired); err != nil {
			return err
		}
	}
	if err := put(tx, LeaseProjection, string(l.ID), leaseRev+1, l); err != nil {
		return err
	}
	raw, err := json.Marshal(next.ID)
	if err != nil {
		return err
	}
	if err := tx.Put(UnitProjection, string(l.Unit), raw); err != nil {
		return err
	}
	return put(tx, AttemptProjection, string(next.ID), 1, next)
}

func (p Projector) changeAttempt(tx *eventstore.Tx, transition string, n types.Attempt) error {
	prev, rev, ok, err := load[types.Attempt](tx, AttemptProjection, string(n.ID))
	if err != nil || !ok {
		return errors.Join(err, fmt.Errorf("attempt %s does not exist", n.ID))
	}
	e := AttemptEvent{Kind: transition, Completion: n.Completion}
	if n.StopReason != nil {
		e.StopReason = *n.StopReason
	}
	if n.LostReason != nil {
		e.LostReason = *n.LostReason
	}
	if n.Checkpoint != nil {
		e.Checkpoint = *n.Checkpoint
	}
	want, err := ApplyAttempt(prev, e)
	if err != nil || !same(want, n) {
		return errors.Join(err, fmt.Errorf("attempt %s is not what %s derives", n.ID, transition))
	}
	if transition == Sealed && !tx.Replaying() {
		if err := p.checkpoint(tx, prev, n); err != nil {
			return err
		}
	}
	return put(tx, AttemptProjection, string(n.ID), rev+1, n)
}

// checkpoint admits a seal's AttemptRecord: a valid record of this attempt,
// its token and base, of the unit's project and task, with every blob it
// names published, sealed for the reason prev, the attempt before the
// seal, derives, and minimal (M1 L1: not pinned, no failing tests, no
// builder notes). It runs at admission only: replay skips it, since it
// reads blobs (record.go).
func (p Projector) checkpoint(tx *eventstore.Tx, prev, a types.Attempt) error {
	if p.Blobs == nil || p.Units == nil {
		return errors.New("a seal needs the blob store and the unit binding to check its checkpoint")
	}
	project, task, ok, err := p.Units.ProjectTask(tx, a.Unit)
	if err != nil || !ok {
		return errors.Join(err, fmt.Errorf("unit %s has no project and task to bind the checkpoint to", a.Unit))
	}
	rc, err := p.Blobs.Open(string(*a.Checkpoint))
	if err != nil {
		return fmt.Errorf("checkpoint %s: %w", *a.Checkpoint, err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	r, err := contract.Decode[types.AttemptRecord]("records/attempt-record", raw)
	if err != nil {
		return fmt.Errorf("checkpoint %s: %w", *a.Checkpoint, err)
	}
	if r.Attempt != a.ID || r.FencingToken != a.FencingToken || r.BaseTree != a.Base.Tree || r.ContractRevision != a.Base.ContractRevision ||
		r.Project != project || r.Task != task {
		return fmt.Errorf("checkpoint %s is not attempt %s's (token %d, base %s)", *a.Checkpoint, a.ID, a.FencingToken, a.Base.Tree)
	}
	reason, err := SealReason(prev)
	if err != nil {
		return err
	}
	if r.Seal.Reason != reason {
		return fmt.Errorf("checkpoint %s is sealed %s, but a %s attempt seals %s", *a.Checkpoint, r.Seal.Reason, prev.Status, reason)
	}
	if r.Pinned || len(r.FailingTests) != 0 || r.BuilderNotes != nil {
		return fmt.Errorf("checkpoint %s is not M1's minimal record: pinned, or holding failing tests or builder notes", *a.Checkpoint)
	}
	named := []types.Digest{r.Trail, r.EvidenceManifest}
	for _, d := range []*types.Digest{r.PartialDiff, r.BuilderNotes} {
		if d != nil {
			named = append(named, *d)
		}
	}
	for _, d := range named {
		if has, err := p.Blobs.Has(string(d)); err != nil || !has {
			return errors.Join(err, fmt.Errorf("checkpoint %s names unpublished blob %s", *a.Checkpoint, d))
		}
	}
	return nil
}
