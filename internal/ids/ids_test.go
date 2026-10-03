// SPDX-License-Identifier: Apache-2.0

package ids

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"go.jetify.com/typeid"
)

// The prefix table of docs/design/m0/02 "Identifiers".
var note02 = []string{"act", "apr", "att", "cal", "chl", "cmd", "cmf", "eff", "ejb", "evt", "exe", "grt", "lea",
	"msr", "nod", "own", "prj", "prp", "rcp", "res", "rg", "run", "src", "tgt", "tsk", "unt"}

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
		"UUIDv4":               {v4.String(), "apr", "not a UUIDv7"},
		"non-RFC 9562 variant": {ncs.String(), "apr", "not a UUIDv7"},
	} {
		t.Run(name, func(t *testing.T) {
			err := Check(c.id, c.prefix)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Check(%q, %q) = %v, want an error containing %q", c.id, c.prefix, err, c.want)
			}
		})
	}
}

func TestParseRefusesABrokenRegistry(t *testing.T) {
	for name, c := range map[string]struct{ defs, want string }{
		"pattern not a prefix": {`{"$defs":{"A":{"$ref":"#/$defs/TypeID","pattern":"^ab"}}}`, "want a ^<prefix>_ pattern"},
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
