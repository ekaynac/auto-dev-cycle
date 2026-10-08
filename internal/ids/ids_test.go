// SPDX-License-Identifier: Apache-2.0

package ids

import (
	"io/fs"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"go.jetify.com/typeid"
)

// The prefix table of docs/design/m0/02 "Identifiers".
var note02 = []string{"act", "apr", "att", "cal", "chl", "cmd", "cmf", "eff", "ejb", "evt", "exe", "grt", "hld", "lea",
	"msr", "nod", "not", "own", "prj", "prp", "rcp", "res", "rg", "run", "sea", "src", "tgt", "tok", "tsk", "unt"}

func TestRegistryMatchesNote02(t *testing.T) {
	got, err := Prefixes()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, note02) {
		t.Fatalf("registered prefixes %v, want note 02's %v", got, note02)
	}
}

func TestNewMintsEveryRegisteredPrefix(t *testing.T) {
	for _, p := range note02 {
		shape := regexp.MustCompile(`^` + p + `_[0-7][0-9a-hjkmnp-tv-z]{25}$`)
		a, err := New(p)
		if err != nil || !shape.MatchString(a) {
			t.Fatalf("New(%s) = %q, %v", p, a, err)
		}
		if err := Check(a, p); err != nil {
			t.Fatalf("Check(New(%s)): %v", p, err)
		}
		if b, _ := New(p); a == b {
			t.Fatalf("New(%s) repeated %s", p, a)
		}
	}
	if _, err := New("xyz"); err == nil {
		t.Fatal("unregistered prefix accepted")
	}
}

func TestCheckRefuses(t *testing.T) {
	apr, _ := New("apr")
	uuid := func(version, variant byte) []byte {
		b := []byte{0x01, 0x8f, 0x3a, 0x10, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd}
		b[6] = version<<4 | b[6]&0x0f
		b[8] = variant<<6 | b[8]&0x3f
		return b
	}
	v4, err := typeid.FromUUIDBytesWithPrefix("apr", uuid(4, 2))
	if err != nil {
		t.Fatal(err)
	}
	ncs, err := typeid.FromUUIDBytesWithPrefix("apr", uuid(7, 0))
	if err != nil {
		t.Fatal(err)
	}
	v7, err := typeid.FromUUIDBytesWithPrefix("apr", uuid(7, 2))
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(v7.String(), "apr"); err != nil {
		t.Fatalf("a constructed UUIDv7 id refused: %v", err)
	}
	for name, c := range map[string]struct{ id, prefix, want string }{
		"wrong prefix":         {apr, "grt", "has prefix"},
		"unregistered prefix":  {apr, "xyz", "unregistered"},
		"not a TypeID":         {"apr_notatypeid", "apr", "not a TypeID"},
		"uppercase":            {strings.ToUpper(apr), "apr", "not a TypeID"},
		"uppercase suffix":     {"apr_" + strings.ToUpper(strings.TrimPrefix(apr, "apr_")), "apr", "not a TypeID"},
		"UUIDv4":               {v4.String(), "apr", "want UUIDv7"},
		"non-RFC 9562 variant": {ncs.String(), "apr", "variant"},
	} {
		t.Run(name, func(t *testing.T) {
			err := Check(c.id, c.prefix)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Check(%q, %q) = %v, want an error containing %q", c.id, c.prefix, err, c.want)
			}
		})
	}
}

// prj opens a defs file with one valid entity id; cases append to it.
const prj = `{"$defs":{"P":{"$ref":"#/$defs/TypeID","pattern":"^prj_"}`

func TestParseRefusesABrokenRegistry(t *testing.T) {
	for name, c := range map[string]struct{ defs, want string }{
		"pattern not a prefix": {`{"$defs":{"A":{"$ref":"#/$defs/TypeID","pattern":"^ab"}}}`, `must be {"$ref"`},
		"mistyped ref":         {prj + `,"X":{"$ref":"#/$defs/TypeId","pattern":"^xyz_"}}}`, `must be {"$ref"`},
		"file-qualified ref":   {prj + `,"X":{"$ref":"defs.json#/$defs/TypeID","pattern":"^xyz_"}}}`, `must be {"$ref"`},
		"ref inside allOf":     {prj + `,"X":{"allOf":[{"$ref":"#/$defs/TypeID"}],"pattern":"^xyz_"}}}`, "must be exactly"},
		"string with a prefix": {prj + `,"X":{"type":"string","pattern":"^xyz_"}}}`, "must be exactly"},
		"prefix too long":      {prj + `,"X":{"$ref":"#/$defs/TypeID","pattern":"^abcd_"}}}`, `must be {"$ref"`},
		"extra keyword":        {prj + `,"X":{"$ref":"#/$defs/TypeID","pattern":"^xyz_","minLength":3}}}`, "must be exactly"},
		"duplicate key":        {`{"$defs":{"P":{"$ref":"#/$defs/TypeID","pattern":"^xyz_"},"P":{"$ref":"#/$defs/TypeID","pattern":"^prj_"}}}`, "duplicate object key"},
		"duplicate prefix":     {`{"$defs":{"A":{"$ref":"#/$defs/TypeID","pattern":"^ab_"},"B":{"$ref":"#/$defs/TypeID","pattern":"^ab_"}}}`, "defined by both"},
		"no entity ids":        {`{"$defs":{"TypeID":{"type":"string"}}}`, "defines no entity ids"},
		"not JSON":             {`{`, "common/defs.json"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parse(fstest.MapFS{"common/defs.json": {Data: []byte(c.defs)}})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("parse = %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// A registry that does not load makes New, Check and Prefixes fail; nothing
// is minted or accepted without it.
func TestLoadErrorIsReported(t *testing.T) {
	origFS := defsFS
	t.Cleanup(func() { defsFS, loadOnce = origFS, new(sync.Once) })
	defsFS = func() fs.FS { return fstest.MapFS{"common/defs.json": {Data: []byte(`{"$defs":{}}`)}} }
	loadOnce = new(sync.Once)
	if _, err := New("prj"); err == nil {
		t.Error("New minted without a registry")
	}
	if err := Check("prj_01m3250vkwffc979nzjw0g2sf9", "prj"); err == nil {
		t.Error("Check accepted without a registry")
	}
	if _, err := Prefixes(); err == nil {
		t.Error("Prefixes listed without a registry")
	}
}
