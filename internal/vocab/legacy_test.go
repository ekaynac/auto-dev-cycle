// SPDX-License-Identifier: Apache-2.0

package vocab

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

const legacyDir = "../../fixtures/legacy/"

type legacyStep struct {
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason"`
}

// legacyGolden is a golden.json as fixtures/legacy/README.md describes it.
type legacyGolden struct {
	Source     string          `json:"source"`
	Class      string          `json:"class"`
	DesignRefs []string        `json:"design_refs"`
	Inputs     json.RawMessage `json:"inputs"`
	Expected   []legacyStep    `json:"expected"`
	Variants   []struct {
		Change   string       `json:"change"`
		Expected []legacyStep `json:"expected"`
	} `json:"variants"`
	ReasonStatus string `json:"reason_status"`
	Notes        string `json:"notes"`
}

// The legacy goldens' setup (docs/design/m0/11 "Legacy incident goldens"):
// each golden is well formed, carries its artifacts, cites design notes that
// exist, and its reason_status is what the registry says of its reasons. The
// loader that runs the goldens through real components is separate (15, L1).
func TestLegacyGoldensSetup(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := os.ReadDir(legacyDir)
	if err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile(legacyDir + "README.md")
	if err != nil {
		t.Fatal(err)
	}
	goldens := map[string]map[string]bool{} // golden → the reasons it uses
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		name := d.Name()
		g, artifacts := readGolden(t, name)
		if !bytes.Contains(readme, []byte("| `"+name+"/` |")) {
			t.Errorf("%s: not listed in the README's contents", name)
		}
		if g.Source == "" || g.Class == "" || g.Notes == "" || len(g.DesignRefs) == 0 || len(g.Expected) == 0 {
			t.Errorf("%s: source, class, design_refs, expected and notes are all required", name)
		}
		if artifacts == 0 {
			t.Errorf("%s: no artifact besides golden.json", name)
		}
		for _, ref := range g.DesignRefs {
			if _, err := os.Stat("../../" + ref); err != nil {
				t.Errorf("%s: design ref %s: %v", name, ref, err)
			}
		}
		steps := slices.Clone(g.Expected)
		for _, v := range g.Variants {
			if v.Change == "" || len(v.Expected) == 0 {
				t.Errorf("%s: a variant needs a change and an expected sequence", name)
			}
			steps = append(steps, v.Expected...)
		}
		used, registered := map[string]bool{}, true
		for _, s := range steps {
			used[s.Reason] = true
			if _, ok := reg.Reasons[s.Reason]; !ok {
				registered = false // proposed; the outcome must still be one of the four
				if _, ok := reg.Outcomes[s.Outcome]; !ok {
					t.Errorf("%s: outcome %q is not one of the four", name, s.Outcome)
				}
				continue
			}
			// A golden states what auto-dev-cycle must write, so the writer's
			// rule applies; a golden carries no detail of its own.
			if err := reg.Check(Result{Outcome: s.Outcome, Reason: types.ReasonCodeOf(s.Reason), Detail: "golden"}); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		if want := map[bool]string{true: "registered", false: "proposed"}[registered]; g.ReasonStatus != want {
			t.Errorf("%s: reason_status %q, want %q", name, g.ReasonStatus, want)
		}
		goldens[name] = used
	}
	if len(goldens) == 0 {
		t.Fatal("no legacy goldens found")
	}
	// A code the registry says a golden introduced is used by that golden.
	for code, x := range reg.Reasons {
		if name, ok := strings.CutPrefix(x.Source, "legacy:"); ok && !goldens[name][code] {
			t.Errorf("reason %s: source %s, which does not use it", code, x.Source)
		}
	}
}

// readGolden decodes dir's golden.json strictly and counts its other files.
func readGolden(t *testing.T, dir string) (legacyGolden, int) {
	t.Helper()
	var g legacyGolden
	b, err := os.ReadFile(filepath.Join(legacyDir, dir, "golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	// ReadStrict refuses duplicate keys and trailing data; UnmarshalStrict
	// refuses unknown fields.
	if _, err := contract.ReadStrict(b); err != nil {
		t.Fatalf("%s/golden.json: %v", dir, err)
	}
	if err := contract.UnmarshalStrict(b, &g); err != nil {
		t.Fatalf("%s/golden.json: %v", dir, err)
	}
	entries, err := os.ReadDir(filepath.Join(legacyDir, dir))
	if err != nil {
		t.Fatal(err)
	}
	return g, len(entries) - 1
}
