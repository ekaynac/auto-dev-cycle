// SPDX-License-Identifier: Apache-2.0

package vocab

import (
	"encoding/json"
	"os"
	"testing"
)

// The schemas check that a reason is registered, not that it may accompany
// its outcome; every (outcome, reason) pair in a valid shared-corpus case
// must still be one a reader accepts, so no example teaches a wrong pair.
func TestCorpusPairs(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../../fixtures/contract/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name  string `json:"name"`
		Doc   any    `json:"doc"`
		Valid bool   `json:"valid"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	n := 0
	var walk func(name string, v any)
	walk = func(name string, v any) {
		switch v := v.(type) {
		case map[string]any:
			o, oOK := v["outcome"].(string)
			code, rOK := v["reason"].(string)
			if oOK && rOK {
				n++
				r, err := ParseReason(code)
				if err == nil {
					// The detail rule is the schemas'; only the pair is checked here.
					err = reg.CheckRead(Result{Outcome: Outcome(o), Reason: r, Detail: "x"})
				}
				if err != nil {
					t.Errorf("%s: %s/%s: %v", name, o, code, err)
				}
			}
			for _, x := range v {
				walk(name, x)
			}
		case []any:
			for _, x := range v {
				walk(name, x)
			}
		}
	}
	for _, c := range cases {
		if c.Valid {
			walk(c.Name, c.Doc)
		}
	}
	if n < 20 {
		t.Fatalf("only %d pairs checked", n)
	}
}
