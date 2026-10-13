// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// operatedStore is an effect store holding a claimed deploy whose
// operation's effect is pending, and a claimed build holding a hold. It
// returns the store's state directory too.
func operatedStore(t *testing.T) (graphStore, string, types.NodeID, Operation) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	g := openEffectStoreAt(t, dir)
	n := g.deployTo("v1", "prod")
	op := g.mustOperate(n)
	b := g.record(g.newNode(types.NodeKindBuild))
	g.must(g.claimOf(b))
	g.must(raised(b, awaitingInput(t), nil))
	return g, dir, n, op
}

func checkHistory(g graphStore) error {
	return g.s.View(context.Background(), CheckHistory)
}

// forgeRecord writes value under key of projection, behind the folds.
func forgeRecord(g graphStore, projection, key string, value any) {
	g.t.Helper()
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return nil, put(tx, projection, key, value) })
}

// effectsSQL changes the effects table behind the store's back: it is no
// projection, so a replay reads it as it is.
func effectsSQL(t *testing.T, dir, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(dir, eventstore.DBFile)}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt, args...); err != nil {
		t.Fatal(err)
	}
}

// A correct history passes; each disagreement between the graph
// projections and the effect and hold records they name is refused
// with ErrHistory, and a projection that cannot be decoded is not.
func TestCheckHistory(t *testing.T) {
	g, _, _, _ := operatedStore(t)
	if err := checkHistory(g); err != nil {
		t.Fatalf("a correct history: %v", err)
	}
	newID := func(prefix string) string {
		id, err := ids.New(prefix)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	for name, c := range map[string]struct {
		fault func(g graphStore, dir string, n types.NodeID, op Operation)
		want  string
	}{
		"an operation naming no effect": {func(g graphStore, _ string, n types.NodeID, op Operation) {
			forgeRecord(g, EffectProjection, string(n), types.NodeOperated{Type: "node_operated", Node: n, Key: *g.state(n).Key, Effect: types.EffectID(newID("eff"))})
		}, "names no effect"},
		"an operation of no node": {func(g graphStore, _ string, n types.NodeID, op Operation) {
			forgeRecord(g, EffectProjection, newID("nod"), types.NodeOperated{Type: "node_operated", Node: n, Key: *g.state(n).Key, Effect: types.EffectID(op.Effect)})
		}, "of no effect node"},
		"an effect on another target": {func(_ graphStore, dir string, _ types.NodeID, op Operation) {
			effectsSQL(t, dir, `UPDATE effects SET target = 'staging' WHERE id = ?`, op.Effect)
		}, "on its target"},
		"an effect for another request": {func(_ graphStore, dir string, _ types.NodeID, op Operation) {
			effectsSQL(t, dir, `UPDATE effects SET request_digest = ? WHERE id = ?`, "sha256:"+strings.Repeat("0", 64), op.Effect)
		}, "request"},
		"a live effect of a completed node": {func(g graphStore, _ string, n types.NodeID, _ Operation) {
			rec, _ := view[nodeRecord](g, NodeProjection, string(n))
			rec.State.State = types.NodeStateStateCompleted
			forgeRecord(g, NodeProjection, string(n), rec)
		}, "moved past its operation"},
		"a completed node whose effect failed": {func(g graphStore, dir string, n types.NodeID, op Operation) {
			effectsSQL(t, dir, `UPDATE effects SET state = 'failed_confirmed' WHERE id = ?`, op.Effect)
			rec, _ := view[nodeRecord](g, NodeProjection, string(n))
			rec.State.State = types.NodeStateStateCompleted
			forgeRecord(g, NodeProjection, string(n), rec)
		}, "completed, but its effect"},
		"an unindexed hold": {func(g graphStore, _ string, _ types.NodeID, _ Operation) {
			g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				keys, err := tx.Keys(HoldProjection, "")
				if err == nil {
					err = tx.Delete(HoldProjection, keys[0])
				}
				return nil, err
			})
		}, "not indexed"},
	} {
		t.Run(name, func(t *testing.T) {
			g, dir, n, op := operatedStore(t)
			c.fault(g, dir, n, op)
			if err := checkHistory(g); !errors.Is(err, ErrHistory) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("refused for another reason: %v", err)
			}
		})
	}
	// A record that cannot be decoded is unreadable (store_unreadable at
	// start), never a refusal of the history.
	for name, forge := range map[string]func(tx *eventstore.Tx, n types.NodeID) error{
		"an operation": func(tx *eventstore.Tx, n types.NodeID) error {
			return tx.Put(EffectProjection, string(n), json.RawMessage(`1`))
		},
		"a hold index entry": func(tx *eventstore.Tx, _ types.NodeID) error {
			keys, err := tx.Keys(HoldProjection, "")
			if err == nil {
				err = tx.Put(HoldProjection, keys[0], json.RawMessage(`1`))
			}
			return err
		},
	} {
		u, _, n, _ := operatedStore(t)
		u.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return nil, forge(tx, n) })
		if err := checkHistory(u); err == nil || errors.Is(err, ErrHistory) {
			t.Fatalf("an undecodable %s: %v", name, err)
		}
	}
}

// An effect's request edited behind the log is invisible to a replay,
// which reads the effects table as it is: only CheckHistory refuses it.
func TestCheckHistorySeesWhatReplayCannot(t *testing.T) {
	g, dir, _, op := operatedStore(t)
	effectsSQL(t, dir, `UPDATE effects SET request_digest = ? WHERE id = ?`, "sha256:"+strings.Repeat("0", 64), op.Effect)
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) > 0 {
		t.Fatalf("the replay saw the edit (%v, %d differences); this test no longer shows what only CheckHistory sees", err, len(diffs))
	}
	if err := checkHistory(g); !errors.Is(err, ErrHistory) {
		t.Fatalf("an effect for another request: %v", err)
	}
}

// Held lists each node that holds, with its project; ProjectOf re-reads it.
func TestHeld(t *testing.T) {
	g, _, _, _ := operatedStore(t)
	var held []NodeHolds
	var project types.ProjectID
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		if held, err = Held(tx); err == nil && len(held) == 1 {
			project, err = ProjectOf(tx, held[0].Node)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || len(held[0].Holds) != 1 || held[0].Holds[0].Hold != "awaiting_input" || held[0].Project != prjID || project != prjID {
		t.Fatalf("held: %+v, project %s", held, project)
	}
}
