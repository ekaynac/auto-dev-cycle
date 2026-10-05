// SPDX-License-Identifier: Apache-2.0

package schemas_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	_ "github.com/ekaynac/auto-dev-cycle/internal/vocab" // installs the reason check
)

// sharedCase is one entry of fixtures/contract/cases.json, the corpus both
// validators classify (docs/design/m0/01 "Code generation"); the TypeScript
// side runs the same file (adapters/contract/test).
type sharedCase struct {
	Name   string          `json:"name"`
	Schema string          `json:"schema"`
	Doc    json.RawMessage `json:"doc"`
	Valid  bool            `json:"valid"`
}

func TestSharedCases(t *testing.T) {
	b, err := os.ReadFile("../fixtures/contract/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []sharedCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no shared cases")
	}
	for _, c := range cases {
		if err := contract.Validate(c.Schema, c.Doc); (err == nil) != c.Valid {
			t.Errorf("%s: valid=%v, got %v", c.Name, c.Valid, err)
		}
	}
}
