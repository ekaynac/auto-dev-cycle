// SPDX-License-Identifier: Apache-2.0

package schemas

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"testing"
)

func TestSetDigestStableAndWellFormed(t *testing.T) {
	a, err := SetDigest()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := SetDigest()
	if a != b {
		t.Fatalf("digest not stable: %s vs %s", a, b)
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(a) {
		t.Fatalf("malformed digest %q", a)
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
