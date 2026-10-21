// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
)

// taskDoneKind is this test's own task end: its fold marks the task done
// in testDoneProjection, in log order, so a replay sees it where the
// append did.
const (
	taskDoneKind       = "test.task_done/v1"
	testDoneProjection = "test_done"
)

// doneTasks admits every task its log has not ended; lenient admits all.
type doneTasks struct{ lenient *bool }

func (d doneTasks) Recordable(tx *eventstore.Tx, task types.TaskID) error {
	if _, done, err := tx.Get(testDoneProjection, string(task)); err != nil || done && !*d.lenient {
		return errors.Join(err, ErrNotAdmitted{Task: task, State: "done"})
	}
	return nil
}

func (d doneTasks) Claimable(tx *eventstore.Tx, task types.TaskID) error {
	return d.Recordable(tx, task)
}

// openTaskStore is openStore whose fold takes tasks, with the test's task
// end registered.
func openTaskStore(t *testing.T, tasks Tasks) graphStore {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	lock, err := eventstore.AcquireLock(state)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	none := func(*eventstore.Tx, eventstore.Command) error { return nil }
	kinds := Kinds()
	maps.Copy(kinds, owedanswers.Kinds())
	maps.Copy(kinds, lease.Kinds())
	kinds[taskDoneKind] = eventstore.Kind{Schema: "records/task-done"}
	ended := func(tx *eventstore.Tx, evs []eventstore.Event) error {
		for _, ev := range evs {
			if ev.Kind == taskDoneKind {
				if err := tx.Put(testDoneProjection, ev.Subject, json.RawMessage(`true`)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	fold := func(tx *eventstore.Tx, evs []eventstore.Event) error { return project(tx, evs, nil, nil, tasks) }
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: kinds, Blobs: blobs,
		Hooks: eventstore.Hooks{Identify: none, Authorize: none, Project: eventstore.ProjectEach(leases(blobs), ended, fold, owedanswers.Project)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); lock.Release() })
	g := graphStore{t: t, s: s, blobs: blobs, conclusions: map[types.Digest]json.RawMessage{}, tree: &struct {
		node types.NodeID
		res  types.ResultID
	}{}, req: new(types.NodeID)}
	g.plan, g.planRes = g.planOf(tskID, prjID)
	return g
}

// Astra #316 M2, through the fold: a review of a task that has ended is
// never replaced. Its review.superseded, which would record the
// replacement, is refused on append; one that reached the log while the
// check was off is refused on replay (RebuildProjections).
func TestASupersessionAfterTheTaskEndsIsRefused(t *testing.T) {
	lenient := false
	g := openTaskStore(t, doneTasks{&lenient})
	review, _, next := g.superseding()
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		b, err := json.Marshal(types.TaskDone{Task: tskID, Project: prjID, As: types.TaskDoneAsCandidate, At: types.Time(time.Now().UTC().Format(time.RFC3339Nano))})
		p := string(prjID)
		return []eventstore.Event{{ID: must(ids.New("evt")), Kind: taskDoneKind, Subject: string(tskID), Project: &p, Payload: b}}, err
	})
	var notAdmitted ErrNotAdmitted
	if err := g.do(supersede(review, next)); !errors.As(err, &notAdmitted) {
		t.Fatalf("a supersession after the task ended: %v", err)
	}
	lenient = true
	g.must(supersede(review, next))
	lenient = false
	if _, err := g.s.RebuildProjections(context.Background()); !errors.As(err, &notAdmitted) {
		t.Errorf("the replay of a supersession after the task ended: %v", err)
	}
}
