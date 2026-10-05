// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// store opens a real event store in dir with one test event kind and an
// authorizer that allows everything (authority is not under test here).
func store(ctx context.Context, dir string) (*eventstore.Store, func(), error) {
	state := filepath.Join(dir, "state")
	lock, err := eventstore.AcquireLock(state)
	if err != nil {
		return nil, nil, err
	}
	blobs, err := blobstore.Open(state)
	if err != nil {
		lock.Release()
		return nil, nil, err
	}
	s, err := eventstore.Open(ctx, lock, eventstore.Config{
		Kinds: eventstore.Kinds{"fixture.appended/v1": {Schema: "common/defs"}}, Blobs: blobs,
		Hooks: eventstore.Hooks{Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil }}})
	if err != nil {
		lock.Release()
		return nil, nil, err
	}
	return s, func() { s.Close(); lock.Release() }, nil
}

// fixtureStore is a store under test with what the fixture observes of it:
// how often a command's apply ran, and the log's head.
type fixtureStore struct {
	ctx     context.Context
	s       *eventstore.Store
	actor   string
	subject string
	applied int // apply callbacks run
}

func open(ctx context.Context, dir string) (*fixtureStore, func(), error) {
	s, done, err := store(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	actor, err := ids.New("act")
	if err == nil {
		var subject string
		if subject, err = ids.New("prj"); err == nil {
			return &fixtureStore{ctx: ctx, s: s, actor: actor, subject: subject}, done, nil
		}
	}
	done()
	return nil, nil, err
}

// submit runs one command appending an event about the subject. Its result
// carries the event's fresh id, so a result that was not replayed from the
// store differs from the stored one.
func (f *fixtureStore) submit(key, input string, expected ...eventstore.Expectation) (eventstore.Outcome, error) {
	cmd, err := ids.New("cmd")
	if err != nil {
		return eventstore.Outcome{}, err
	}
	ev, err := ids.New("evt")
	if err != nil {
		return eventstore.Outcome{}, err
	}
	return f.s.Submit(f.ctx, eventstore.Command{ID: cmd, Actor: f.actor, IdempotencyKey: key, Input: []byte(input), Expected: expected},
		func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			f.applied++
			return []eventstore.Event{{ID: ev, Kind: "fixture.appended/v1", Subject: f.subject, Payload: []byte(`{}`)}}, json.RawMessage(`{"event":"` + ev + `"}`), nil
		})
}

// head is the log's last seq.
func (f *fixtureStore) head() (int64, error) {
	h, err := f.s.Head(f.ctx)
	return h.Seq, err
}

// observe is one step's result and the state after it: the step, then
// whether each assertion held; a violated one fails the fixture.
type observe struct {
	steps      []Step
	violations []string
}

// step records a command's step; an error the writer does not name is the
// fixture not running.
func (o *observe) step(err error) error {
	st, err := commandStep(err)
	if err != nil {
		return err
	}
	o.steps = append(o.steps, st)
	return nil
}

func (o *observe) check(ok bool, format string, a ...any) {
	if !ok {
		o.violations = append(o.violations, fmt.Sprintf("step %d: ", len(o.steps))+fmt.Sprintf(format, a...))
	}
}

// committed checks the head and the apply count after a step.
func (o *observe) committed(f *fixtureStore, head int64, applied int) error {
	h, err := f.head()
	if err != nil {
		return err
	}
	o.check(h == head, "the log's head is %d, want %d", h, head)
	o.check(f.applied == applied, "apply ran %d times, want %d", f.applied, applied)
	return nil
}

// idempotencyKeyReuse: the same key and input twice, then the key with
// another input (11: "idempotency key reuse with a different input"). The
// repeat replays the first command (its id and stored result) without
// running it again; the refusal runs nothing and writes nothing.
func idempotencyKeyReuse(ctx context.Context, dir string) ([]Step, []string, error) {
	f, done, err := open(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer done()
	var o observe
	first, err := f.submit("k", `{"n":1}`)
	if err = o.step(err); err != nil {
		return nil, nil, err
	}
	o.check(!first.Replayed && first.FirstSeq == 1, "the first command was not applied as seq 1")
	if err := o.committed(f, 1, 1); err != nil {
		return nil, nil, err
	}
	again, err := f.submit("k", `{"n":1}`)
	if err = o.step(err); err != nil {
		return nil, nil, err
	}
	o.check(again.Replayed, "the repeat was not a replay")
	o.check(again.CommandID == first.CommandID, "the replay names command %s, want %s", again.CommandID, first.CommandID)
	o.check(string(again.Result) == string(first.Result), "the replay's result %s differs from the stored %s", again.Result, first.Result)
	if err := o.committed(f, 1, 1); err != nil {
		return nil, nil, err
	}
	_, err = f.submit("k", `{"n":2}`)
	if err = o.step(err); err != nil {
		return nil, nil, err
	}
	if err := o.committed(f, 1, 1); err != nil {
		return nil, nil, err
	}
	return o.steps, o.violations, nil
}

// staleExpectedState: a command against version 0, a second against the
// now-stale version 0, a third against version 1, the one the first made
// (11: "stale expected state"). The refusal runs nothing and writes nothing.
func staleExpectedState(ctx context.Context, dir string) ([]Step, []string, error) {
	f, done, err := open(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer done()
	at := func(v int64) eventstore.Expectation { return eventstore.Expectation{Subject: f.subject, Version: v} }
	var o observe
	for _, c := range []struct {
		key     string
		version int64
		head    int64
		applied int
	}{{"k1", 0, 1, 1}, {"k2", 0, 1, 1}, {"k3", 1, 2, 2}} {
		_, err := f.submit(c.key, `{"k":"`+c.key+`"}`, at(c.version))
		if err = o.step(err); err != nil {
			return nil, nil, err
		}
		if err := o.committed(f, c.head, c.applied); err != nil {
			return nil, nil, err
		}
	}
	return o.steps, o.violations, nil
}
