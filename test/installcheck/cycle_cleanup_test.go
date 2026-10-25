package installcheck

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Opus #441 M2: every variable a driver's cleanup reads is named in the
// comment above it ("# cleanup's inputs: ...") and assigned at the top level
// before its trap, so none of them is ever the caller's (an exported root
// once made a run remove its caller's directory; Opus #441 H1). A variable
// cleanup gains without both fails here.
func TestCleanupReadsOnlyItsOwnVariables(t *testing.T) {
	use := regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)|\(\(\s*!?([A-Za-z_][A-Za-z0-9_]*)\s*\)\)`)
	for _, script := range []string{"../../scripts/installed-cycle.sh", "../../scripts/installed-cycle-set.sh"} {
		b, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		trap := slices.Index(lines, "trap cleanup EXIT")
		start := slices.Index(lines, "cleanup() {")
		if trap < 0 || start < 0 || start > trap {
			t.Fatalf("%s: no cleanup() before its trap", script)
		}
		end := start + slices.Index(lines[start:], "}")
		var listed []string
		for _, l := range lines[:start] {
			if s, ok := strings.CutPrefix(l, "# cleanup's inputs: "); ok {
				listed = strings.Fields(strings.SplitN(s, "(", 2)[0])
			}
		}
		var locals, reads []string
		for _, l := range lines[start+1 : end] {
			if s, ok := strings.CutPrefix(strings.TrimSpace(l), "local "); ok {
				locals = append(locals, strings.Fields(s)...)
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(l), "#") {
				continue
			}
			for _, m := range use.FindAllStringSubmatch(l, -1) {
				if v := m[1] + m[2]; !slices.Contains(locals, v) && !slices.Contains(reads, v) {
					reads = append(reads, v)
				}
			}
		}
		slices.Sort(reads)
		sortedListed := slices.Sorted(slices.Values(listed))
		if len(reads) == 0 || !slices.Equal(reads, sortedListed) {
			t.Errorf("%s: cleanup reads %v, its comment lists %v", script, reads, listed)
		}
		for _, v := range reads {
			assigned := regexp.MustCompile(`(^|\s)` + v + `=`)
			if !slices.ContainsFunc(lines[:start], func(l string) bool {
				return l != "" && l[0] != ' ' && l[0] != '\t' && l[0] != '#' && assigned.MatchString(l)
			}) {
				t.Errorf("%s: cleanup reads %s, which is not assigned at the top level before its trap", script, v)
			}
		}
	}
}
