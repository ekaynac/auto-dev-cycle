// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
)

// counted are counting's projections, each at version 1.
var counted = []Projection{{Name: "kinds", Version: 1}, {Name: "subjects", Version: 1}, {Name: "recent", Version: 1}}

// projectionsOnly is rows without the record of built projections.
func projectionsOnly(rows map[string]string) map[string]string {
	out := maps.Clone(rows)
	maps.DeleteFunc(out, func(k, _ string) bool { return len(k) > len(builtRecord) && k[:len(builtRecord)+1] == builtRecord+"/" })
	return out
}

// #226 entry 107: a projection a store never built, here subjects in a store
// written before the record existed with kinds and recent only, is built
// from the log, equal to a full replay, and every projection is recorded.
// Built once, it is never built again: emptied, it is refused, and nothing
// is kept.
func TestBuildProjectionsBuildsAnAbsentProjection(t *testing.T) {
	dir := stateDir(t)
	e := openProjected(t, dir, counting(nil))
	populate(t, e)
	full := liveRows(t, dir)
	mustExec(t, raw(t, dir), `DELETE FROM proj_kv WHERE projection = 'subjects'`)
	built, diffs, err := e.s.BuildProjections(context.Background(), counted, []string{"kinds", "recent"})
	if err != nil || len(diffs) != 0 || !slices.Equal(built, []string{"subjects"}) {
		t.Fatalf("built %v, diffs %+v, %v", built, diffs, err)
	}
	after := liveRows(t, dir)
	if fmt.Sprint(projectionsOnly(after)) != fmt.Sprint(full) {
		t.Fatalf("the built projections are not a full replay:\n%v\n%v", projectionsOnly(after), full)
	}
	for _, p := range counted {
		if after[builtRecord+"/"+p.Name] != `{"version":1}` {
			t.Errorf("%s is not recorded as built: %v", p.Name, after)
		}
	}
	// The scratch table is gone once the build commits (Opus #341 L4 S1).
	var scratch int
	if err := e.s.w.QueryRow(`SELECT count(*) FROM temp.sqlite_master WHERE name = 'proj_rebuild'`).Scan(&scratch); err != nil || scratch != 0 {
		t.Fatalf("the scratch table outlived the build: %d, %v", scratch, err)
	}
	if diffs, err := e.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("a rebuild after the build: %v, %+v", err, diffs)
	}
	built, diffs, err = e.s.BuildProjections(context.Background(), counted, []string{"kinds", "recent"})
	if err != nil || len(diffs) != 0 || len(built) != 0 {
		t.Fatalf("a second build: built %v, diffs %+v, %v", built, diffs, err)
	}
	mustExec(t, raw(t, dir), `DELETE FROM proj_kv WHERE projection = 'subjects'`)
	emptied := liveRows(t, dir)
	built, diffs, err = e.s.BuildProjections(context.Background(), counted, []string{"kinds", "recent"})
	if err != nil || len(diffs) == 0 || built != nil {
		t.Fatalf("a built projection emptied since: built %v, diffs %+v, %v", built, diffs, err)
	}
	if fmt.Sprint(liveRows(t, dir)) != fmt.Sprint(emptied) {
		t.Fatal("a refused build changed the store")
	}
}

// A store with the record, written by a build that folded kinds alone,
// builds the projections a later build registers, equal to a full replay.
func TestBuildProjectionsAfterTheRecord(t *testing.T) {
	dir := stateDir(t)
	all := counting(nil)
	e := openProjected(t, dir, func(tx *Tx, evs []Event) error {
		if err := all(tx, evs); err != nil {
			return err
		}
		for _, p := range counted[1:] {
			keys, err := tx.Keys(p.Name, "")
			if err != nil {
				return err
			}
			for _, k := range keys {
				if err := tx.Delete(p.Name, k); err != nil {
					return err
				}
			}
		}
		return nil
	})
	populate(t, e)
	if built, diffs, err := e.s.BuildProjections(context.Background(), counted[:1], nil); err != nil || len(diffs) != 0 || len(built) != 0 {
		t.Fatalf("the first build: built %v, diffs %+v, %v", built, diffs, err)
	}
	e.close()
	e = openProjected(t, dir, counting(nil))
	built, diffs, err := e.s.BuildProjections(context.Background(), counted, []string{"kinds"})
	if err != nil || len(diffs) != 0 || !slices.Equal(built, []string{"subjects", "recent"}) {
		t.Fatalf("projections registered later: built %v, diffs %+v, %v", built, diffs, err)
	}
	full := liveRows(t, dir)
	if diffs, err := e.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 || len(filter(full, "subjects/")) == 0 {
		t.Fatalf("the built projections are not a full replay: %v, %+v, %v", err, diffs, full)
	}
}

// A damaged projection the store holds, assumed, registered and held but
// never recorded, or never registered, is refused, never built over, and
// nothing is kept: not the absent projection's build (subjects), not the
// record.
func TestBuildProjectionsRefusesDrift(t *testing.T) {
	for name, c := range map[string]struct {
		stmt    string
		assumed []string
	}{
		"changed":            {`UPDATE proj_kv SET value = '99' WHERE projection = 'kinds' AND key = 'test.appended/v1'`, []string{"kinds", "recent"}},
		"emptied, assumed":   {`DELETE FROM proj_kv WHERE projection = 'recent'`, []string{"kinds", "recent"}},
		"held, not recorded": {`UPDATE proj_kv SET value = '"x"' WHERE projection = 'recent'`, []string{"kinds"}},
		"forged":             {`INSERT INTO proj_kv(projection, key, value) VALUES ('stray', 'x', '1')`, []string{"kinds", "recent"}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := stateDir(t)
			e := openProjected(t, dir, counting(nil))
			populate(t, e)
			mustExec(t, raw(t, dir), `DELETE FROM proj_kv WHERE projection = 'subjects'`, c.stmt)
			before := liveRows(t, dir)
			built, diffs, err := e.s.BuildProjections(context.Background(), counted, c.assumed)
			if err != nil || len(diffs) == 0 || built != nil {
				t.Fatalf("built %v, diffs %+v, %v", built, diffs, err)
			}
			if fmt.Sprint(liveRows(t, dir)) != fmt.Sprint(before) {
				t.Fatal("a refused build changed the store")
			}
		})
	}
}

// A registration that is not a projection name, once, at a version from 1,
// or an assumed projection never registered, is refused; so is a record of
// a build that does not decode, at version 0 or under a name that is not a
// projection name (Opus #341 L4 S2), never read as absent.
func TestBuildProjectionsRefusals(t *testing.T) {
	dir := stateDir(t)
	e := openProjected(t, dir, counting(nil))
	populate(t, e)
	for name, reg := range map[string][]Projection{
		"name":      {{Name: builtRecord, Version: 1}},
		"version":   {{Name: "kinds", Version: 0}},
		"duplicate": {{Name: "kinds", Version: 1}, {Name: "kinds", Version: 2}},
	} {
		if _, _, err := e.s.BuildProjections(context.Background(), reg, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, _, err := e.s.BuildProjections(context.Background(), counted, []string{"stray"}); err == nil {
		t.Error("an unregistered assumed projection: accepted")
	}
	mustExec(t, raw(t, dir), `DELETE FROM proj_kv WHERE projection = 'subjects'`)
	for name, row := range map[string]string{"version 0": `'subjects', '{"version":0}'`, "not json": `'subjects', 'x'`, "bad name": `'Bad Name', '{"version":1}'`} {
		mustExec(t, raw(t, dir), `INSERT INTO proj_kv(projection, key, value) VALUES ('`+builtRecord+`', `+row+`)`)
		if _, _, err := e.s.BuildProjections(context.Background(), counted, nil); !errors.Is(err, ErrBuildRecord) {
			t.Errorf("%s: %v", name, err)
		}
		mustExec(t, raw(t, dir), `DELETE FROM proj_kv WHERE projection = '`+builtRecord+`'`)
	}
}

// Opus #341 H2: the assumed projections are taken as built whatever the
// record holds, so a forged record of another projection never makes one
// of them a projection to build: emptied, it is refused, and nothing is
// kept.
func TestBuildProjectionsAssumedAlwaysBuilt(t *testing.T) {
	dir := stateDir(t)
	e := openProjected(t, dir, counting(nil))
	populate(t, e)
	mustExec(t, raw(t, dir), `DELETE FROM proj_kv WHERE projection = 'recent'`,
		`INSERT INTO proj_kv(projection, key, value) VALUES ('`+builtRecord+`', 'subjects', '{"version":1}')`)
	before := liveRows(t, dir)
	built, diffs, err := e.s.BuildProjections(context.Background(), counted, []string{"kinds", "recent"})
	if err != nil || len(diffs) == 0 || built != nil {
		t.Fatalf("built %v, diffs %+v, %v", built, diffs, err)
	}
	if fmt.Sprint(liveRows(t, dir)) != fmt.Sprint(before) {
		t.Fatal("a refused build changed the store")
	}
}

// A record of a projection not registered is a difference (Opus #341 L1).
// A recorded version above the registered one is ErrBuildRecord: a later
// build wrote the store (L2). One below rebuilds nothing, and is raised.
func TestBuildProjectionsRecordVersions(t *testing.T) {
	dir := stateDir(t)
	e := openProjected(t, dir, counting(nil))
	populate(t, e)
	if _, diffs, err := e.s.BuildProjections(context.Background(), counted, nil); err != nil || len(diffs) != 0 {
		t.Fatalf("the first build: %+v, %v", diffs, err)
	}
	mustExec(t, raw(t, dir), `INSERT INTO proj_kv(projection, key, value) VALUES ('`+builtRecord+`', 'zzz', '{"version":1}')`)
	built, diffs, err := e.s.BuildProjections(context.Background(), counted, nil)
	if err != nil || built != nil || len(diffs) != 1 || diffs[0].Projection != builtRecord || diffs[0].Key != "zzz" || diffs[0].Rebuilt != nil {
		t.Fatalf("an unregistered record: built %v, diffs %+v, %v", built, diffs, err)
	}
	mustExec(t, raw(t, dir), `DELETE FROM proj_kv WHERE projection = '`+builtRecord+`' AND key = 'zzz'`)
	later := []Projection{counted[0], counted[1], {Name: "recent", Version: 2}}
	if _, _, err := e.s.BuildProjections(context.Background(), later, nil); err != nil {
		t.Fatal(err)
	}
	if got := liveRows(t, dir)[builtRecord+"/recent"]; got != `{"version":2}` {
		t.Fatalf("the record was not raised: %s", got)
	}
	before := liveRows(t, dir)
	if _, _, err := e.s.BuildProjections(context.Background(), counted, nil); !errors.Is(err, ErrBuildRecord) {
		t.Fatalf("a store a later build wrote: %v", err)
	}
	if fmt.Sprint(liveRows(t, dir)) != fmt.Sprint(before) {
		t.Fatal("a refused build changed the store")
	}
}

// A store opened with Config.Projections compares its record of built
// projections on a rebuild, as a start does (Opus #342 L1): a record of a
// projection not registered is a difference, one that does not decode is
// ErrBuildRecord. Opened without, the record is left out. An invalid
// Config.Projections is refused at open.
func TestRebuildComparesTheRecord(t *testing.T) {
	dir := stateDir(t)
	e := openRegistered(t, dir, counting(nil), counted)
	populate(t, e)
	if _, diffs, err := e.s.BuildProjections(context.Background(), counted, nil); err != nil || len(diffs) != 0 {
		t.Fatalf("the build: %+v, %v", diffs, err)
	}
	if diffs, err := e.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("a recorded store: %+v, %v", diffs, err)
	}
	mustExec(t, raw(t, dir), `INSERT INTO proj_kv(projection, key, value) VALUES ('`+builtRecord+`', 'zzz', '{"version":1}')`)
	diffs, err := e.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) != 1 || !diffs[0].BuildRecord() || diffs[0].Key != "zzz" || *diffs[0].Live != `{"version":1}` || diffs[0].Rebuilt != nil {
		t.Fatalf("an unregistered record: %+v, %v", diffs, err)
	}
	e.close()
	bare := openProjected(t, dir, counting(nil))
	if diffs, err := bare.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("without Config.Projections: %+v, %v", diffs, err)
	}
	bare.close()
	mustExec(t, raw(t, dir), `UPDATE proj_kv SET value = 'x' WHERE projection = '`+builtRecord+`' AND key = 'zzz'`)
	e = openRegistered(t, dir, counting(nil), counted)
	if _, err := e.s.RebuildProjections(context.Background()); !errors.Is(err, ErrBuildRecord) {
		t.Fatalf("an undecodable record: %v", err)
	}
	e.close()
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if s, err := Open(context.Background(), lock, Config{Kinds: testKinds, Blobs: e.blobs, Projections: []Projection{{Name: "kinds", Version: 0}}}); err == nil {
		s.Close()
		t.Fatal("an invalid Config.Projections was accepted")
	}
}
