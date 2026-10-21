// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
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
// a build that does not decode, never read as absent.
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
	mustExec(t, raw(t, dir), `INSERT INTO proj_kv(projection, key, value) VALUES ('`+builtRecord+`', 'subjects', '{"version":0}')`)
	mustExec(t, raw(t, dir), `DELETE FROM proj_kv WHERE projection = 'subjects'`)
	if _, _, err := e.s.BuildProjections(context.Background(), counted, nil); err == nil {
		t.Error("an undecodable record: accepted")
	}
}
