// SPDX-License-Identifier: Apache-2.0

package capability

import (
	"bufio"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
// capability's actions at most, and this build activates intake, answers
// and admission only (P22a-3).
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
	if !slices.Equal(active, []string{"intake", "answers", "admission"}) {
		t.Errorf("active: %v", active)
	}
	if ks := EffectKinds(); len(ks) != 0 {
		t.Errorf("registered effect kinds before launch: %v", ks)
	}
	if ks := NodeKinds(); len(ks) != 0 {
		t.Errorf("node kinds added before merge: %v", ks)
	}
	if _, ok := Gating("grant.create"); ok {
		t.Error("an ungated action is gated")
	}
	if waiting := (Capability{Rows: of("I2", Admission), Pending: []string{"a prerequisite"}}); waiting.Active() {
		t.Error("a capability with a pending prerequisite is active")
	}
	// P22a-6d: review misses only I5's and I13's admission, which enforce
	// under launch alone and are kept by launch's activation (Opus 22a-6d
	// L1), and stays inactive on its P31 Pending (the qualification on
	// record).
	review, _ := Of("review")
	if review.Active() || !slices.Equal(review.Missing(), []Row{{"I5", Admission}, {"I13", Admission}}) ||
		len(review.Pending) != 0 || !slices.Equal(review.Proven, []string{ReviewQualified}) {
		t.Errorf("review: missing %v, pending %v, proven %v", review.Missing(), review.Pending, review.Proven)
	}
	// The qualification is derived at adcd's start (ActiveWith): unchecked
	// or failing, it stays open with its reason; passed, it clears, and
	// review stays inactive on its rows alone.
	if active, open := review.ActiveWith(nil); active || !slices.Equal(open, []string{ReviewQualified + ": not checked"}) {
		t.Errorf("review unchecked: %v %v", active, open)
	}
	if active, open := review.ActiveWith(map[string]error{ReviewQualified: errors.New("no record of the route")}); active ||
		!slices.Equal(open, []string{ReviewQualified + ": no record of the route"}) {
		t.Errorf("review unqualified: %v %v", active, open)
	}
	if active, open := review.ActiveWith(map[string]error{ReviewQualified: nil}); active || len(open) != 0 {
		t.Errorf("review qualified: %v %v", active, open)
	}
	proven := Capability{Rows: of("I2", Admission), Proven: []string{"a proof"}}
	if active, _ := proven.ActiveWith(map[string]error{"a proof": nil}); proven.Active() || !active {
		t.Errorf("a proven capability: static %v, derived %v", proven.Active(), active)
	}
	if active, _ := (Capability{Rows: of("I2", Admission), Pending: []string{"open"}, Proven: []string{"a proof"}}).ActiveWith(map[string]error{"a proof": nil}); active {
		t.Error("a capability proven but still pending is active")
	}
	// The table's static readers (Gating, EffectKinds, NodeKinds) never
	// decide a capability with Proven prerequisites: it wires none of them
	// from the table (the start's derived value alone decides it).
	for _, c := range Table {
		if len(c.Proven) > 0 && len(c.Actions)+len(c.EffectKinds)+len(c.NodeKinds) > 0 {
			t.Errorf("%s has Proven prerequisites and wires from the static table: %v %v %v", c.Name, c.Actions, c.EffectKinds, c.NodeKinds)
		}
	}
	// K1 (launch's kept bits, #226 entry 164): launch misses I10's and
	// I12's admission and consumption and I17's (K2), and I5's admission
	// and I21's (the activation PR, last): it is not active.
	launch, _ := Of("launch")
	want := []Row{{"I5", Admission}, {"I10", Admission}, {"I10", Fold}, {"I10", Consumption}, {"I10", Effect}, {"I12", Admission}, {"I12", Consumption},
		{"I17", Admission}, {"I17", Consumption}, {"I17", Effect}, {"I21", Admission}, {"I21", Fold}, {"I21", Effect}}
	if launch.Active() || !slices.Equal(launch.Missing(), want) {
		t.Errorf("launch: missing %v", launch.Missing())
	}
	// Opus #409 L1: I6's bits are its model_call half; the merge
	// capabilities stay inactive on P20's merge half, their Pending, while
	// it is open.
	for _, name := range []string{"merge-local", "merge-github"} {
		c, _ := Of(name)
		if c.Active() || !slices.ContainsFunc(c.Pending, func(p string) bool { return strings.HasPrefix(p, "P20's merge half") }) {
			t.Errorf("%s: active %v, pending %v", name, c.Active(), c.Pending)
		}
	}
}

// requiredBy is the inventory summary's "Required by" column, row for row
// (01-invariant-inventory.md: derived from §1's table), as rows per
// capability. A cell is "`a`, `b`: spec" parts joined by "; ": spec is
// "all", "all but the GitHub effect check" (no effect row), a list of
// checks, or "the … rows" (all four: the inventory keeps model_call and
// merge in the same rows); "every other capability" is each one the cell
// does not name.
func requiredBy(t *testing.T) map[string][]Row {
	t.Helper()
	raw, err := os.ReadFile("../../docs/design/m1/01-invariant-inventory.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile(`^\| (I[0-9]+) [^|]*\|[^|]*\|[^|]*\| (.*) \|$`)
	name, paren := regexp.MustCompile("`([a-z-]+)`"), regexp.MustCompile(`\([^)]*\)`)
	checks := map[string]string{"admission": Admission, "fold": Fold, "consumption": Consumption, "effect": Effect}
	out := map[string][]Row{}
	for _, line := range strings.Split(string(raw), "\n") {
		m := row.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		named := map[string]bool{}
		for _, part := range strings.Split(m[2], "; ") {
			who, spec, ok := strings.Cut(part, ": ")
			if !ok {
				t.Fatalf("%s: %q", m[1], part)
			}
			spec = strings.TrimSpace(paren.ReplaceAllString(spec, ""))
			var rows []Row
			switch {
			case spec == "all", strings.HasPrefix(spec, "the ") && strings.Contains(spec, " rows"):
				rows = all(m[1])
			case spec == "all but the GitHub effect check":
				rows = of(m[1], Admission, Fold, Consumption)
			default:
				for _, w := range strings.Split(spec, ", ") {
					c, ok := checks[w]
					if !ok {
						t.Fatalf("%s: check %q", m[1], w)
					}
					rows = append(rows, Row{m[1], c})
				}
			}
			var caps []string
			if who == "every other capability" {
				for _, c := range Table {
					if !named[c.Name] {
						caps = append(caps, c.Name)
					}
				}
			} else {
				for _, x := range name.FindAllStringSubmatch(who, -1) {
					caps, named[x[1]] = append(caps, x[1]), true
				}
			}
			for _, c := range caps {
				out[c] = append(out[c], rows...)
			}
		}
	}
	return out
}

// TestRowsAreTheInventory (Opus #298 M1): each capability requires exactly
// the rows the inventory's "Required by" column gives it.
func TestRowsAreTheInventory(t *testing.T) {
	sorted := func(rs []Row) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Invariant+" "+r.Check)
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	doc := requiredBy(t)
	for _, c := range Table {
		if got, want := sorted(c.Rows), sorted(doc[c.Name]); !slices.Equal(got, want) {
			t.Errorf("%s requires %v; the inventory says %v", c.Name, got, want)
		}
		delete(doc, c.Name)
	}
	if len(doc) != 0 {
		t.Errorf("capabilities the inventory names and the table does not: %v", doc)
	}
}

// Set.Keeping builds a TEST table only: no non-test Go file of the
// repository calls it, so the bits adcd reads are this build's (Built)
// and a capability is active only once its PR sets them.
func TestKeepingIsTestOnly(t *testing.T) {
	err := filepath.WalkDir("../..", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == "node_modules" || d.Name() == "dist" || strings.HasPrefix(d.Name(), ".")) && p != "../..":
			return filepath.SkipDir
		case d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go"):
			return nil
		}
		b, err := os.ReadFile(p)
		if err == nil && bytes.Contains(b, []byte(".Keeping(")) && !strings.HasSuffix(filepath.ToSlash(p), "internal/capability/capability.go") {
			t.Errorf("%s builds a capability table of its own (Keeping)", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// A test build keeps what it is given, and the build's table is
	// unchanged by it.
	launch, _ := Of("launch")
	more := Built().Keeping(launch.Missing()...)
	if l, _ := more.Of("launch"); !l.Active() || slices.Contains(more.EffectKinds(), "model_call") != true {
		t.Errorf("a test build keeping launch's rows: active %v, effect kinds %v", l.Active(), more.EffectKinds())
	}
	if len(launch.Missing()) > 0 {
		if l, _ := Of("launch"); l.Active() || slices.Contains(EffectKinds(), "model_call") {
			t.Error("a test build changed the build's table")
		}
	}
	if c, _ := more.Gating("task.raise_rail"); !c.Active() {
		t.Error("the test build's gating reads the build's bits")
	}
}
