// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"os"
	"strings"
	"testing"
)

const sample = `# T

## I1. One

| Check | Today | Status | Item |
|---|---|---|---|
| admission | a | kept | — |
| fold/replay | b | GAP | P1 |

## I2. Two

| Check | Today | Status | Item |
|---|---|---|---|
| admission | c | kept + GAP | P2 |
| effect | d | n/a | — |

## Count

` + Begin + `
stale figures
` + End + `
History, with no figures.
`

// Rewrite counts the invariants and their rows by status, and writes the
// line between the markers alone; a status it does not know, or markers
// missing or repeated, are refused.
func TestRewrite(t *testing.T) {
	out, err := Rewrite([]byte(sample))
	want := strings.Replace(sample, "stale figures", "4 rows across 2 invariants. **2 rows have a gap**: 1 are GAP and 1 are kept + GAP. 1 are kept and 1 are n/a.", 1)
	if err != nil || string(out) != want {
		t.Fatalf("rewrite: %v\n%s", err, out)
	}
	for name, doc := range map[string]string{
		"an unknown status":    strings.Replace(sample, "| n/a |", "| maybe |", 1),
		"no markers":           strings.Replace(sample, Begin, "", 1),
		"a repeated marker":    sample + End + "\n",
		"markers out of order": strings.Replace(strings.Replace(sample, Begin, "X", 1), End, Begin, 1),
	} {
		if _, err := Rewrite([]byte(doc)); err == nil {
			t.Errorf("%s was rewritten", name)
		}
	}
}

// The committed inventory's count line is its tables' count: a change of
// a row's status needs make gen.
func TestTheInventoryCountIsGenerated(t *testing.T) {
	doc, err := os.ReadFile("../../" + Path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Rewrite(doc)
	if err != nil || string(out) != string(doc) {
		t.Fatalf("the inventory's count line is stale; run make gen (%v)", err)
	}
}
