// SPDX-License-Identifier: Apache-2.0

package schemas

import (
	"encoding/json"
	"io/fs"
	"reflect"
	"testing"
	"testing/fstest"
)

// The expected value was computed outside Go:
//
//	printf 'a/x.json\0%s\nb/c/y.json\0%s\n' "$(printf '{}' | sha256sum | cut -c1-64)" \
//	  "$(printf '[1]' | sha256sum | cut -c1-64)" | sha256sum
func TestDigestOfMatchesTheFraming(t *testing.T) {
	got, err := DigestOf(fstest.MapFS{
		"b/c/y.json":   {Data: []byte("[1]")},
		"a/x.json":     {Data: []byte("{}")},
		"a/readme.md":  {Data: []byte("not a schema")},
		"b/c/notes.go": {Data: []byte("package c")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "sha256:d82741bce6b16eda887516f7067963bd62502f6f2bbbc2372cbfcf5685924477"; got != want {
		t.Fatalf("DigestOf = %s, want %s", got, want)
	}
}

// The embedded inventory is exactly the schema files of this release; adding
// one means listing it here and in the go:embed directive.
func TestEmbeddedInventory(t *testing.T) {
	paths, err := Paths()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"common/defs.json", "records/instrument-contract.json", "records/receipt.json"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("embedded schemas = %v, want %v", paths, want)
	}
	a, _ := SetDigest()
	b, _ := DigestOf(FS())
	if a != b {
		t.Fatalf("SetDigest %s differs from DigestOf(FS()) %s", a, b)
	}
}

// Every file declares draft 2020-12 and an $id equal to Base + its path, so
// cross-file $refs resolve to the embedded bytes and nothing else.
func TestEveryFileIDMatchesItsPath(t *testing.T) {
	paths, err := Paths()
	if err != nil || len(paths) == 0 {
		t.Fatalf("no embedded schemas: %v", err)
	}
	for _, p := range paths {
		b, err := fs.ReadFile(FS(), p)
		if err != nil {
			t.Fatal(err)
		}
		var head struct {
			Schema string `json:"$schema"`
			ID     string `json:"$id"`
		}
		if err := json.Unmarshal(b, &head); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if head.Schema != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("%s: $schema = %q", p, head.Schema)
		}
		if head.ID != Base+p {
			t.Errorf("%s: $id = %q, want %q", p, head.ID, Base+p)
		}
	}
}
