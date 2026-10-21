// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// refused asserts the start is refused with reason and a detail containing
// want, and that the refusal released the lock.
func refused(t *testing.T, dir string, cfg Config, outcome vocab.Outcome, reason vocab.Reason, want string) {
	t.Helper()
	_, err := Start(context.Background(), cfg)
	startErr(t, err, outcome, reason)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("refused for another reason: %v", err)
	}
	lockFree(t, dir)
}

// lastSeq is the closed store's last event seq.
func lastSeq(t *testing.T, dir string) (seq int64) {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(dir, eventstore.DBFile)}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

func leaseStore(t *testing.T) (string, Config) {
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t, tokens(10)))
	sys := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	sys.CreatedBy = sys.ID
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sys) })
	w.grantedLease(sys.ID, 4)
	w.done()
	return dir, Config{StateDir: dir, Groups: []types.ResourceGroup{tokens(10)}}
}

// execSQL changes the closed store's database behind the store's back.
func execSQL(t *testing.T, dir string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(dir, eventstore.DBFile)}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

// A trusted head that is not a non-negative seq and a sha256 hash is
// refused by a direct Start too, before the state directory is created.
func TestStartValidatesTrustedHead(t *testing.T) {
	for _, h := range []eventstore.Head{{Seq: -1, Hash: "sha256:" + strings.Repeat("a", 64)}, {Seq: 0, Hash: "invalid"}} {
		dir := stateDir(t)
		if _, err := Start(context.Background(), Config{StateDir: dir, TrustedHead: &h}); !errors.Is(err, ErrConfig) {
			t.Fatalf("trusted head %+v accepted: %v", h, err)
		}
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the state directory was created: %v", err)
		}
	}
}

// A lease projection that cannot be decoded is store_unreadable, never
// reported as a grant from before capacity was wired.
func TestMalformedLeaseProjectionUnreadable(t *testing.T) {
	dir, cfg := leaseStore(t)
	execSQL(t, dir, `INSERT INTO proj_kv(projection, key, value) VALUES ('`+lease.LeaseProjection+`', 'lea_malformed', '{"record":1}')`)
	refused(t, dir, cfg, vocab.CouldNotRun, vocab.ReasonStoreUnreadable, "projections cannot be read")
}

// A store with events but no actor gets no system actor: Bootstrap writes
// only an empty store's first event.
func TestEventsWithoutActorRefused(t *testing.T) {
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t))
	w.do(types.ActorID(newID(t, "act")), func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		e, err := ledger.Measure(types.UsageMeasurement{ID: types.MeasurementID(newID(t, "msr")), Call: types.CallID(newID(t, "cal")),
			Attempt: types.AttemptID(newID(t, "att")), Lease: types.LeaseID(newID(t, "lea")), Kind: types.UsageMeasurementKindDelta, Sequence: 1,
			Tokens: types.UsageMeasurementTokens{In: 1, Out: 1}, Turns: 1, Milliseconds: 1,
			Cost:          types.UsageMeasurementCost{AmountMicros: 1, Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
			SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport})
		return []eventstore.Event{e}, err
	})
	w.done()
	refused(t, dir, Config{StateDir: dir}, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "no actor")
}

// An otherwise empty store holding an orphan effect gets no system actor:
// Bootstrap refuses it, and the refused start appends nothing.
func TestOrphanEffectRefused(t *testing.T) {
	dir := stateDir(t)
	seed(t, dir, daemonProjection(t)).done()
	execSQL(t, dir, `INSERT INTO effects(id, kind, command, target, request_digest, state, attempts, last_observation)
		VALUES ('eff_orphan', 'model_call', 'cmd_orphan', 'target', 'sha256:x', 'pending', 0, '')`)
	refused(t, dir, Config{StateDir: dir}, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "no actor")
	if lastSeq(t, dir) != 0 {
		t.Fatal("a refused start appended to the log")
	}
}

func TestTrustedHeadChecked(t *testing.T) {
	dir, cfg := leaseStore(t)
	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	head := d.Head()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.TrustedHead = &head
	d, err = Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("the trusted head was refused: %v", err)
	}
	d.Close()
	cfg.TrustedHead = &eventstore.Head{Seq: head.Seq + 5, Hash: head.Hash}
	refused(t, dir, cfg, vocab.Failed, vocab.ReasonLogIntegrityBroken, "verification")
}

// A store whose projections match the log starts, and starts again.
func TestCleanStoreStarts(t *testing.T) {
	_, cfg := leaseStore(t)
	for range 2 {
		d, err := Start(context.Background(), cfg)
		if err != nil {
			t.Fatalf("a clean store was refused: %v", err)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// A persisted projection that differs from a replay of the log, by a forged,
// a changed or a missing record, is refused before the system actor is
// recorded, and is not repaired.
func TestProjectionDriftRefused(t *testing.T) {
	for name, stmt := range map[string]string{
		"forged":  `INSERT INTO proj_kv(projection, key, value) VALUES ('capacity_reserved', 'forged', '{}')`,
		"changed": `UPDATE proj_kv SET value = '{}' WHERE projection = 'leases'`,
		"missing": `DELETE FROM proj_kv WHERE projection = 'capacity_reserved'`,
	} {
		t.Run(name, func(t *testing.T) {
			dir, cfg := leaseStore(t)
			execSQL(t, dir, stmt)
			head := lastSeq(t, dir)
			for range 2 {
				refused(t, dir, cfg, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "differ from a replay")
			}
			if lastSeq(t, dir) != head {
				t.Fatal("a refused start appended to the log")
			}
		})
	}
}

// rows are the closed store's records of projection, by key.
func rows(t *testing.T, dir, projection string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(dir, eventstore.DBFile)}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, err := db.Query(`SELECT key, value FROM proj_kv WHERE projection = ?`, projection)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out := map[string]string{}
	for r.Next() {
		var k, v string
		if err := r.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// startClose starts the daemon on cfg, checks that its projections are a
// full replay of the log, and closes it.
func startClose(t *testing.T, cfg Config) {
	t.Helper()
	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("the start was refused: %v", err)
	}
	diffs, err := d.store.RebuildProjections(context.Background())
	if err := errors.Join(err, d.Close()); err != nil || len(diffs) != 0 {
		t.Fatalf("the started store is not a full replay: %+v, %v", diffs, err)
	}
}

// #226 entry 107: a store main wrote before #328, holding a granted lease
// but no lease_grants, starts: lease_grants is built from the log, equal
// to a full replay, and every projection is recorded as built. It starts
// again.
func TestStoreBeforeLeaseGrantsStarts(t *testing.T) {
	dir, cfg := leaseStore(t)
	want := rows(t, dir, lease.GrantProjection)
	if len(want) == 0 {
		t.Fatal("the granted lease left no lease_grants record")
	}
	execSQL(t, dir, `DELETE FROM proj_kv WHERE projection = '`+lease.GrantProjection+`'`)
	if len(rows(t, dir, "_built")) != 0 {
		t.Fatal("a store written before the record holds one")
	}
	startClose(t, cfg)
	if got := rows(t, dir, lease.GrantProjection); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("lease_grants built %v, want %v", got, want)
	}
	if got := rows(t, dir, "_built"); len(got) != len(projections) || got[lease.GrantProjection] != `{"version":1}` {
		t.Fatalf("the record of built projections: %v", got)
	}
	startClose(t, cfg)
}

// A projection the store built, emptied since, is refused, never built
// again: lease_grants, built at a first start, and capacity_reserved, which
// a store written before the record is taken to have built.
func TestEmptiedProjectionRefused(t *testing.T) {
	dir, cfg := leaseStore(t)
	startClose(t, cfg)
	for _, projection := range []string{lease.GrantProjection, capacity.ReservedProjection} {
		t.Run(projection, func(t *testing.T) {
			sub, cfg := leaseStore(t)
			if projection == lease.GrantProjection {
				sub = dir
				cfg.StateDir = dir
			}
			execSQL(t, sub, `DELETE FROM proj_kv WHERE projection = '`+projection+`'`)
			for range 2 {
				refused(t, sub, cfg, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "differ from a replay")
			}
			if len(rows(t, sub, projection)) != 0 {
				t.Fatal("a refused start built the projection")
			}
		})
	}
}

// A damaged projection is refused even when the store lacks lease_grants,
// and the refused start keeps nothing: lease_grants is not built, nothing
// is recorded.
func TestDamagedStoreBeforeLeaseGrantsRefused(t *testing.T) {
	dir, cfg := leaseStore(t)
	execSQL(t, dir, `DELETE FROM proj_kv WHERE projection = '`+lease.GrantProjection+`'`,
		`UPDATE proj_kv SET value = json_set(value, '$.forged', 1) WHERE projection = '`+lease.LeaseProjection+`'`)
	refused(t, dir, cfg, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "differ from a replay")
	if len(rows(t, dir, lease.GrantProjection)) != 0 || len(rows(t, dir, "_built")) != 0 {
		t.Fatal("a refused start kept a build or its record")
	}
}

// #226 entry 107 on #333: a store main wrote before #333, holding a node and
// an unended lease but no unit_open, graph_task_nodes or lease_grants,
// starts: each is built from the log, equal to the rows a store written
// with them holds, and to a full replay. Main refuses that store.
func TestStoreBefore333Starts(t *testing.T) {
	dir, cfg := graphStore(t)
	added := []string{lease.GrantProjection, lease.OpenProjection, graph.TaskNodeProjection}
	want := map[string]map[string]string{}
	for _, name := range added {
		if want[name] = rows(t, dir, name); len(want[name]) == 0 {
			t.Fatalf("the store holds no %s record", name)
		}
		execSQL(t, dir, `DELETE FROM proj_kv WHERE projection = '`+name+`'`)
	}
	startClose(t, cfg)
	for _, name := range added {
		if got := rows(t, dir, name); fmt.Sprint(got) != fmt.Sprint(want[name]) {
			t.Errorf("%s built %v, want %v", name, got, want[name])
		}
	}
	startClose(t, cfg)
}

// Opus #341 H2: a forged record of built projections never makes a
// projection of builtBeforeRecord one to build. One forged row in a store
// written before the record, with capacity_reserved emptied, is refused;
// so is a store holding a lease granted before capacity was wired, every
// projection but identity's emptied, actors recorded: the build would
// write capacity_granted for a lease capacity never admitted, and
// capacity's history check would pass from then on. Nothing is kept.
func TestForgedRecordBuildsNoAssumedProjection(t *testing.T) {
	t.Run("one forged row", func(t *testing.T) {
		dir, cfg := leaseStore(t)
		execSQL(t, dir, `DELETE FROM proj_kv WHERE projection = '`+capacity.ReservedProjection+`'`,
			`INSERT INTO proj_kv(projection, key, value) VALUES ('_built', '`+lease.GrantProjection+`', '{"version":1}')`)
		refused(t, dir, cfg, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "differ from a replay")
		if len(rows(t, dir, capacity.ReservedProjection)) != 0 || len(rows(t, dir, "_built")) != 1 {
			t.Fatal("a refused start kept a build or its record")
		}
	})
	t.Run("granted before capacity", func(t *testing.T) {
		dir := stateDir(t)
		w := seed(t, dir, leasesOnly)
		w.grantedLease(types.ActorID(newID(t, "act")), 4)
		w.done()
		execSQL(t, dir, `DELETE FROM proj_kv WHERE projection NOT IN ('`+identity.ActorProjection+`', '`+identity.TokenProjection+`', '`+identity.BootstrapProjection+`')`,
			`INSERT INTO proj_kv(projection, key, value) VALUES ('_built', '`+identity.ActorProjection+`', '{"version":1}')`)
		refused(t, dir, Config{StateDir: dir, Groups: []types.ResourceGroup{tokens(10)}}, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "differ from a replay")
		if len(rows(t, dir, capacity.GrantedProjection)) != 0 || len(rows(t, dir, lease.LeaseProjection)) != 0 {
			t.Fatal("a refused start built a projection")
		}
	})
}

// A record of built projections that names a projection this build does not
// fold is a difference with its own detail (Opus #341 L1, #342 L1). One
// that records a later version than this build folds (a later build wrote
// the store, L2), or that does not decode (L3, L4), is store_unreadable,
// never read as absent.
func TestBuildRecordRefused(t *testing.T) {
	for name, c := range map[string]struct {
		stmt   string
		reason vocab.Reason
		detail string
	}{
		"unknown":     {`INSERT INTO proj_kv(projection, key, value) VALUES ('_built', 'zzz', '{"version":1}')`, vocab.ReasonAmbiguousState, "names a projection this build does not fold"},
		"later":       {`UPDATE proj_kv SET value = '{"version":99}' WHERE projection = '_built' AND key = 'leases'`, vocab.ReasonStoreUnreadable, "record of built projections cannot be read"},
		"undecodable": {`UPDATE proj_kv SET value = 'x' WHERE projection = '_built' AND key = 'leases'`, vocab.ReasonStoreUnreadable, "record of built projections cannot be read"},
		"bad name":    {`INSERT INTO proj_kv(projection, key, value) VALUES ('_built', 'Bad Name', '{"version":1}')`, vocab.ReasonStoreUnreadable, "record of built projections cannot be read"},
	} {
		t.Run(name, func(t *testing.T) {
			dir, cfg := leaseStore(t)
			startClose(t, cfg)
			execSQL(t, dir, c.stmt)
			before := rows(t, dir, "_built")
			refused(t, dir, cfg, vocab.CouldNotRun, c.reason, c.detail)
			if fmt.Sprint(rows(t, dir, "_built")) != fmt.Sprint(before) {
				t.Fatal("a refused start rewrote the record")
			}
		})
	}
}

// The daemon's store is opened with its projections (Opus #342 L1): a
// rebuild reports a record of a projection this build does not fold, the
// difference the next start refuses, so an operator following the refusal
// sees it.
func TestRebuildReportsUnknownRecord(t *testing.T) {
	dir, cfg := leaseStore(t)
	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, dir, `INSERT INTO proj_kv(projection, key, value) VALUES ('_built', 'zzz', '{"version":1}')`)
	diffs, err := d.store.RebuildProjections(context.Background())
	if err := errors.Join(err, d.Close()); err != nil || len(diffs) != 1 || !diffs[0].BuildRecord() || diffs[0].Key != "zzz" {
		t.Fatalf("the rebuild: %+v, %v", diffs, err)
	}
	refused(t, dir, cfg, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "names a projection this build does not fold")
}

// A record at a later version than this build folds is refused at start
// (store_unreadable) and is ErrBuildRecord on the daemon's rebuild, so the
// two agree (Mythos #342 L2).
func TestRebuildReportsLaterRecord(t *testing.T) {
	dir, cfg := leaseStore(t)
	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, dir, `UPDATE proj_kv SET value = '{"version":99}' WHERE projection = '_built' AND key = 'leases'`)
	_, err = d.store.RebuildProjections(context.Background())
	if cerr := d.Close(); cerr != nil || !errors.Is(err, eventstore.ErrBuildRecord) {
		t.Fatalf("the rebuild: %v, %v", err, cerr)
	}
	refused(t, dir, cfg, vocab.CouldNotRun, vocab.ReasonStoreUnreadable, "record of built projections cannot be read")
}

// declaredProjections are the projection names the code declares: every
// string constant named *Projection outside tests and the test guard's
// fakes, which projector does not compose.
func declaredProjections(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := filepath.Join("..")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path == filepath.Join(root, "testguard") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				v := spec.(*ast.ValueSpec)
				for i, n := range v.Names {
					if !strings.HasSuffix(n.Name, "Projection") || i >= len(v.Values) {
						continue
					}
					if lit, ok := v.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						name, err := strconv.Unquote(lit.Value)
						if err != nil {
							return err
						}
						out[name] = f.Name.Name + "." + n.Name
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// projections registers exactly the projections the code declares, whether
// or not a history writes them (Opus #341 M1), and every projection a full
// history leaves a record in. builtBeforeRecord is frozen at #341's base;
// projections are those, then the ones added since (#328, #333).
func TestProjectionsRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, p := range projections {
		registered[p.Name] = true
	}
	declared := declaredProjections(t)
	if len(declared) < 50 {
		t.Fatalf("%d projections declared: the scan missed some", len(declared))
	}
	for name, decl := range declared {
		if !registered[name] {
			t.Errorf("projection %s (%s) is not registered", name, decl)
		}
	}
	for name := range registered {
		if _, ok := declared[name]; !ok {
			t.Errorf("registered projection %s is not declared", name)
		}
	}
	dir, _ := graphStore(t)
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(dir, eventstore.DBFile)}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, err := db.Query(`SELECT DISTINCT projection FROM proj_kv`)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for r.Next() {
		var name string
		if err := r.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if !registered[name] {
			t.Errorf("projection %s is not registered", name)
		}
	}
	frozen := []string{"actors", "tokens", "identity_bootstrap", "grants", "grant_ids", "grant_lineage", "grant_commands", "seat",
		"proposals", "proposals_pending", "notes", "projects", "tasks", "owed_inputs", "leases", "attempts", "unit_attempts",
		"attempt_stops", "lease_targets", "lease_restores", "lease_restore_cycles", "attempt_refusals", "capacity_reserved",
		"capacity_granted", "capacity_liabilities", "capacity_liable_calls", "usage_measurements", "usage_measurement_ids",
		"model_calls", "model_call_leases", "model_call_floors", "graph_nodes", "graph_claims", "graph_keys", "graph_reuses",
		"graph_renewals", "graph_successors", "graph_tasks", "graph_results", "graph_gens", "graph_holds", "graph_retries",
		"graph_effects", "graph_observed", "graph_attempt_results", "graph_attempts", "graph_generations", "graph_requirements",
		"graph_preempted"}
	if fmt.Sprint(builtBeforeRecord) != fmt.Sprint(frozen) {
		t.Fatalf("builtBeforeRecord changed:\n%v\n%v", builtBeforeRecord, frozen)
	}
	var names []string
	for _, p := range projections {
		names = append(names, p.Name)
	}
	if want := append(frozen, "lease_grants", "unit_open", "graph_task_nodes"); fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("projections are not builtBeforeRecord, then the ones added since:\n%v\n%v", names, want)
	}
}

// A persisted event of a kind or version this build does not read is
// refused, not skipped.
func TestUnsupportedEventVersionRefused(t *testing.T) {
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t), "actor.created/v9")
	a := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	a.CreatedBy = a.ID
	w.do(a.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := identity.CreateActor(a)
		for i := range evs {
			evs[i].Kind = "actor.created/v9"
		}
		return evs, err
	})
	w.done()
	refused(t, dir, Config{StateDir: dir}, vocab.CouldNotRun, vocab.ReasonStoreUnreadable, "does not read")
}

// CheckHistory runs on the existing projections before any replay: on a
// store with both a lease capacity never admitted and drifted projections,
// its refusal is the one reported.
func TestCapacityHistoryCheckedBeforeReplay(t *testing.T) {
	dir := stateDir(t)
	w := seed(t, dir, leasesOnly)
	w.grantedLease(types.ActorID(newID(t, "act")), 4)
	w.done()
	execSQL(t, dir, `INSERT INTO proj_kv(projection, key, value) VALUES ('stray', 'forged', '{}')`)
	refused(t, dir, Config{StateDir: dir, Groups: []types.ResourceGroup{tokens(10)}}, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "before capacity was wired")
}

// The configured groups must still be the ones the recorded grants
// reserved: a group gone, or reclassified as an evidence target, refuses
// the start by name, before any replay.
func TestCapacityGroupsCheckedAtStart(t *testing.T) {
	dir, cfg := leaseStore(t)
	target := tokens(1)
	target.Kind, target.ReleaseRule, target.CapacityModel.Unit = types.ResourceGroupKindEvidenceTarget, types.ResourceGroupReleaseRuleOnRestoreVerified, types.CapacityUnitSlots
	for name, groups := range map[string][]types.ResourceGroup{"gone": {}, "reclassified": {target}} {
		c := cfg
		c.Groups = groups
		refused(t, dir, c, vocab.CouldNotRun, vocab.ReasonAmbiguousState, string(quota))
		_, err := Start(context.Background(), c)
		if err == nil || !strings.Contains(err.Error(), "previous kind") {
			t.Errorf("%s: the refusal does not say how to recover: %v", name, err)
		}
	}
	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

// A failed restart transition refuses the start and appends nothing. A
// fresh store runs it too (#226 entry 64: the model calls' Recorder needs
// it on every open store): its failure refuses that start, after the
// system actor's event alone, which the next start reads as its store.
func TestFailedRestartTransitionRefused(t *testing.T) {
	markPending = func(*eventstore.Store, context.Context, string) (int, error) { return 0, errors.New("injected") }
	t.Cleanup(func() { markPending = (*eventstore.Store).MarkPendingUncertain })
	dir, cfg := leaseStore(t)
	head := lastSeq(t, dir)
	refused(t, dir, cfg, vocab.CouldNotRun, vocab.ReasonStoreUnreadable, "marked uncertain")
	if lastSeq(t, dir) != head {
		t.Fatal("a refused start appended to the log")
	}
	fresh := stateDir(t)
	if _, err := Start(context.Background(), Config{StateDir: fresh}); err == nil || lastSeq(t, fresh) != 1 {
		t.Fatalf("a fresh start whose transition failed: %v, log at %d", err, lastSeq(t, fresh))
	}
	markPending = (*eventstore.Store).MarkPendingUncertain
	d, err := Start(context.Background(), Config{StateDir: fresh})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

// A replay the projector refuses is ambiguous_state; one that cannot run,
// here on a cancelled context, is store_unreadable.
func TestReplayErrorsClassified(t *testing.T) {
	dir := stateDir(t)
	refuse := false
	w := seed(t, dir, func(blobs lease.Blobs, holds map[string]types.Hold) func(*eventstore.Tx, []eventstore.Event) error {
		project := daemonProjection(t, tokens(10))(blobs, holds)
		return func(tx *eventstore.Tx, evs []eventstore.Event) error {
			if refuse {
				return errors.New("injected refusal")
			}
			return project(tx, evs)
		}
	})
	defer w.done()
	fullHistory(t, w)
	refuse = true
	startErr(t, checkProjections(context.Background(), w.s), vocab.CouldNotRun, vocab.ReasonAmbiguousState)
	refuse = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	startErr(t, checkProjections(ctx, w.s), vocab.CouldNotRun, vocab.ReasonStoreUnreadable)
}

// fullHistory leaves a record in every projection the daemon composes, the
// waiting lease holding its hold; a change to any of them is drift.
func TestFullHistoryProjected(t *testing.T) {
	dir, _ := fullStore(t)
	held := `(SELECT key FROM proj_kv WHERE projection = '` + lease.LeaseProjection + `' AND value LIKE '%"quota_reset"%')`
	for projection, want := range map[string]int{identity.ActorProjection: 3, authority.GrantProjection: 1, lease.LeaseProjection: 2,
		capacity.ReservedProjection: 1, capacity.GrantedProjection: 1, ledger.UsageProjection: 1, ledger.MeasurementProjection: 1, "held": 1} {
		q := `SELECT count(*) FROM proj_kv WHERE projection = '` + projection + `'`
		if projection == "held" {
			q = `SELECT count(*) FROM ` + held + ``
		}
		if got := count(t, dir, q); got != want {
			t.Errorf("%s: %d records, want %d", projection, got, want)
		}
	}
	for _, projection := range []string{identity.ActorProjection, authority.GrantProjection, ledger.UsageProjection, "held"} {
		t.Run(projection, func(t *testing.T) {
			sub, cfg := fullStore(t)
			key := `(SELECT min(key) FROM proj_kv WHERE projection = '` + projection + `')`
			if projection == "held" {
				projection, key = lease.LeaseProjection, held
			}
			execSQL(t, sub, `UPDATE proj_kv SET value = json_set(value, '$.forged', 1) WHERE projection = '`+projection+`' AND key = `+key)
			refused(t, sub, cfg, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "differ from a replay")
		})
	}
}

func fullStore(t *testing.T) (string, Config) {
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t, tokens(10)))
	fullHistory(t, w)
	w.done()
	return dir, Config{StateDir: dir, Groups: []types.ResourceGroup{tokens(10)}}
}

func count(t *testing.T, dir, q string) (n int) {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(dir, eventstore.DBFile)}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStateDirPermissionsRefused(t *testing.T) {
	dir := stateDir(t)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Start(context.Background(), Config{StateDir: dir})
	startErr(t, err, vocab.CouldNotRun, vocab.ReasonStoreUnreadable)
	if !strings.Contains(err.Error(), "mode 0700") {
		t.Fatalf("refused for another reason: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(stateDir(t), link); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(context.Background(), Config{StateDir: link}); err == nil {
		t.Fatal("a symlinked state directory was accepted")
	}
}

// A fresh store gets one persisted system actor, which later starts load,
// and its bootstrap secret's record (bootstrap_test.go).
func TestSystemActorPersisted(t *testing.T) {
	cfg := Config{StateDir: stateDir(t)}
	var heads []int64
	for range 2 {
		d, err := Start(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		heads = append(heads, d.Head().Seq)
		var system int
		if err := d.store.View(context.Background(), func(tx *eventstore.Tx) error {
			keys, err := tx.Keys(identity.ActorProjection, "")
			system = len(keys)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if system != 1 {
			t.Fatalf("%d actors after start", system)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if heads[0] != 0 || heads[1] != 2 {
		t.Fatalf("the system actor was not created once: heads %v", heads)
	}
}

// The system actor is created through the trusted startup path; every
// ordinary submission after it, even one naming the system actor, is still
// refused.
func TestRefuseAllAfterSystemActor(t *testing.T) {
	d, err := Start(context.Background(), Config{StateDir: stateDir(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var sys string
	if err := d.store.View(context.Background(), func(tx *eventstore.Tx) error {
		keys, err := tx.Keys(identity.ActorProjection, "")
		if len(keys) == 1 {
			sys = keys[0]
		}
		return err
	}); err != nil || sys == "" {
		t.Fatalf("no system actor after start: %v", err)
	}
	// Without a principal nobody gets past the identity recheck; the
	// system principal does, and no action is wired for it.
	noop := eventstore.Applies(func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		return nil, json.RawMessage(`{}`), nil
	})
	for _, actor := range []string{sys, newID(t, "act")} {
		cmd := newID(t, "cmd")
		_, err := d.store.SubmitApplied(context.Background(), eventstore.Command{ID: cmd, Actor: actor, Action: "test.command", IdempotencyKey: "adcd-system-actor", Input: []byte(`{}`)}, noop)
		if !errors.Is(err, identity.ErrUnauthenticated) {
			t.Fatalf("a submission as %s without a principal was not refused: %v", actor, err)
		}
	}
	// Startup took the store's one system claim: nothing after it can.
	if _, err := d.store.ClaimSystem(); !errors.Is(err, eventstore.ErrSystemClaimed) {
		t.Fatalf("the system claim is still free after start: %v", err)
	}
	if d.system.Actor() != types.ActorID(sys) {
		t.Fatalf("the daemon holds %s's principal, not the system actor's", d.system.Actor())
	}
	cmd := newID(t, "cmd")
	if _, err := d.store.SubmitApplied(context.Background(), d.system.Command(cmd, "review.request", cmd, []byte(`{}`), nil), noop); !errors.Is(err, ErrCommandsRefused) {
		t.Fatalf("the system principal's command was not refused: %v", err)
	}
	// note.add is wired: its handler checks the scope, and refuses a note
	// by the system, which is no note author.
	note := d.system.Command(cmd, authority.NoteAction, cmd, []byte(`{}`), nil)
	var invalid *contract.Error
	if _, err := d.store.Submit(context.Background(), note, authority.AddNote(note)); !errors.As(err, &invalid) {
		t.Fatalf("note.add is not wired: %v", err)
	}
	before, err := d.store.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	note.Input = []byte(`{"project":"prj_01m3250vkwffc979nzjw0g2sa1","body":"x"}`)
	_, err = d.store.Submit(context.Background(), note, authority.AddNote(note))
	var r *authority.Refusal
	if after, herr := d.store.Head(context.Background()); !errors.As(err, &r) || r.Reason != vocab.ReasonActorKindRefused || herr != nil || after != before {
		t.Fatalf("the system's note: %v", err)
	}
}
