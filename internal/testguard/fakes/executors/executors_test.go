// SPDX-License-Identifier: Apache-2.0

package executors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

func TestFamily(t *testing.T) {
	in := map[types.ExecutorID]string{"exe-claude": "claude", "exe-gpt": "gpt-5.1"}
	r, err := New(in)
	if err != nil {
		t.Fatal(err)
	}
	in["exe-claude"], in["exe-new"] = "gpt", "gpt" // the caller's map is not the registry
	for id, want := range map[types.ExecutorID]string{
		"exe-claude": "claude",
		"exe-gpt":    "gpt-5.1",
		"exe-new":    Unknown, // added to the caller's map after New
		"exe-absent": Unknown,
	} {
		if got := r.Family(id); got != want {
			t.Errorf("Family(%s) = %q, want %q", id, got, want)
		}
	}
	if got := (Registry{}).Family("exe-claude"); got != Unknown {
		t.Errorf("an empty registry gives %q, want %q", got, Unknown)
	}
}

// A family that is not a receipt's family name is refused, not normalised.
func TestNewRefusesBadFamilies(t *testing.T) {
	for _, f := range []string{"", "Claude", "CLAUDE", " claude", "claude\n", "-claude", "unknown", "cl aude"} {
		if _, err := New(map[types.ExecutorID]string{"exe-a": "claude", "exe-b": f}); err == nil {
			t.Errorf("New accepted the family %q", f)
		}
	}
}

// FamilyPattern is the receipt schema's model-family name pattern.
func TestFamilyPatternIsTheSchemas(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "schemas", "records", "receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs struct {
			Name struct {
				Pattern string `json:"pattern"`
			} `json:"Name"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Defs.Name.Pattern != FamilyPattern.String() {
		t.Errorf("FamilyPattern %q, the receipt schema's %q", FamilyPattern, doc.Defs.Name.Pattern)
	}
}
