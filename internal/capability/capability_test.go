// SPDX-License-Identifier: Apache-2.0

package capability

import (
	"bufio"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// inventory is each row's status in docs/design/m1/01-invariant-inventory.md.
func inventory(t *testing.T) map[Row]string {
	t.Helper()
	f, err := os.Open("../../docs/design/m1/01-invariant-inventory.md")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	heading := regexp.MustCompile(`^## (I[0-9]+)\. `)
	out, inv := map[Row]string{}, ""
	s := bufio.NewScanner(f)
	s.Buffer(nil, 1<<20)
	for s.Scan() {
		line := s.Text()
		if m := heading.FindStringSubmatch(line); m != nil {
			inv = m[1]
			continue
		}
		cells := strings.Split(line, "|")
		if inv == "" || len(cells) != 6 {
			continue
		}
		check := strings.TrimSpace(cells[1])
		if slices.Contains([]string{Admission, Fold, Consumption, Effect}, check) {
			out[Row{inv, check}] = strings.TrimSpace(cells[3])
		}
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestKeptIsTheInventory (P22a-2, W7): every required row's bit is set
// exactly when the inventory keeps the row (kept, or n/a); every bit is a
// required row's; every row a capability names is in the inventory.
func TestKeptIsTheInventory(t *testing.T) {
	status := inventory(t)
	if len(status) != 88 {
		t.Fatalf("the inventory has %d rows, not 88", len(status))
	}
	required := map[Row]bool{}
	for _, c := range Table {
		for _, r := range c.Rows {
			if _, ok := status[r]; !ok {
				t.Errorf("%s: %v is not an inventory row", c.Name, r)
			}
			required[r] = true
		}
	}
	for r := range required {
		if st := status[r]; Kept(r) != (st == "kept" || st == "n/a") {
			t.Errorf("%v: bit %v, the inventory says %q", r, Kept(r), st)
		}
	}
	for r := range kept {
		if !required[r] {
			t.Errorf("%v has a bit and no capability requires it", r)
		}
	}
}

// TestTable: names are unique, I15 gates every capability (all four rows
// but intake's admission alone), an action or effect kind belongs to one
// capability's actions at most, and this build activates intake and answers
// only.
func TestTable(t *testing.T) {
	seen, actions := map[string]bool{}, map[string]string{}
	var active []string
	for _, c := range Table {
		if seen[c.Name] {
			t.Errorf("%s twice", c.Name)
		}
		seen[c.Name] = true
		var i15 []Row
		for _, r := range c.Rows {
			if r.Invariant == "I15" {
				i15 = append(i15, r)
			}
		}
		if want := all("I15"); c.Name == "intake" {
			if !slices.Equal(i15, of("I15", Admission)) {
				t.Errorf("intake's I15 rows: %v", i15)
			}
		} else if !slices.Equal(i15, want) {
			t.Errorf("%s's I15 rows: %v", c.Name, i15)
		}
		for _, a := range c.Actions {
			if other, dup := actions[a]; dup {
				t.Errorf("%s is %s's and %s's", a, other, c.Name)
			}
			actions[a] = c.Name
			if g, ok := Gating(a); !ok || g.Name != c.Name {
				t.Errorf("Gating(%s): %s", a, g.Name)
			}
		}
		if c.Active() {
			active = append(active, c.Name)
		}
		if got, ok := Of(c.Name); !ok || got.Name != c.Name {
			t.Errorf("Of(%s)", c.Name)
		}
	}
	if !slices.Equal(active, []string{"intake", "answers"}) {
		t.Errorf("active: %v", active)
	}
	if ks := EffectKinds(); len(ks) != 0 {
		t.Errorf("registered effect kinds before launch: %v", ks)
	}
	if _, ok := Gating("grant.create"); ok {
		t.Error("an ungated action is gated")
	}
	if waiting := (Capability{Rows: of("I2", Admission), Pending: []string{"a prerequisite"}}); waiting.Active() {
		t.Error("a capability with a pending prerequisite is active")
	}
	review, _ := Of("review")
	if review.Active() || len(review.Missing()) == 0 {
		t.Errorf("review: missing %v", review.Missing())
	}
}
