// SPDX-License-Identifier: Apache-2.0

package installcheck

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// cleanupProblems reads a driver's text: every variable its cleanup reads
// must be named in the comment above it ("# cleanup's inputs: ...") and
// assigned at the top level before its trap, each such assignment a literal
// ("", a number, or a command's output), never one that keeps an inherited
// value (name="${name:-...}"); and cleanup must read nothing this parse
// cannot follow (an indirect ${!x}, eval, a nameref).
func cleanupProblems(text string) []string {
	use := regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)|\(\(\s*!?([A-Za-z_][A-Za-z0-9_]*)\s*\)\)`)
	// The whole assignment, up to a blank, ; or the line's end: "", a number,
	// a literal word, or a command's output "$(...)" (Astra #441 r2 Low 3).
	literal := `(""|[0-9]+|[A-Za-z0-9_./:-]+|"\$\([^)]*\)")(\s|;|$)`
	lines := strings.Split(text, "\n")
	trap := slices.Index(lines, "trap cleanup EXIT")
	start := slices.Index(lines, "cleanup() {")
	if trap < 0 || start < 0 || start > trap {
		return []string{"no cleanup() before its trap"}
	}
	end := start + slices.Index(lines[start:], "}")
	var problems, listed, locals, reads []string
	for _, l := range lines[:start] {
		if s, ok := strings.CutPrefix(l, "# cleanup's inputs: "); ok {
			listed = strings.Fields(strings.SplitN(s, "(", 2)[0])
		}
	}
	for _, l := range lines[start+1 : end] {
		for _, opaque := range []string{"${!", "eval ", "declare -n", "local -n", "typeset -n"} {
			if strings.Contains(l, opaque) {
				problems = append(problems, fmt.Sprintf("cleanup reads through %q, which this parse cannot follow", opaque))
			}
		}
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
	if len(reads) == 0 || !slices.Equal(reads, slices.Sorted(slices.Values(listed))) {
		problems = append(problems, fmt.Sprintf("cleanup reads %v, its comment lists %v", reads, listed))
	}
	for _, v := range reads {
		assigned := regexp.MustCompile(`(^|\s)` + v + `=`)
		whole := regexp.MustCompile(`^` + v + `=` + literal)
		found := false
		for _, l := range lines[:start] {
			if l == "" || l[0] == ' ' || l[0] == '\t' || l[0] == '#' {
				continue
			}
			for _, m := range assigned.FindAllStringIndex(l, -1) {
				found = true
				at := strings.TrimLeft(l[m[0]:], " \t")
				if !whole.MatchString(at) {
					problems = append(problems, fmt.Sprintf("%s is assigned %s before cleanup's trap, not a literal", v, strings.Fields(at)[0][len(v)+1:]))
				}
			}
		}
		if !found {
			problems = append(problems, fmt.Sprintf("cleanup reads %s, which is not assigned at the top level before its trap", v))
		}
	}
	return problems
}

// Opus #441 M2, r2 N1: both drivers' cleanups read only their own
// variables (cleanupProblems), so none of them is ever the caller's (an
// exported root once made a run remove its caller's directory; Opus #441
// H1); and each way a cleanup could hide an input from this parse is
// caught. TestInstalledCycleRefusesWithoutPodman's victim run is the
// dynamic row.
func TestCleanupReadsOnlyItsOwnVariables(t *testing.T) {
	for _, script := range []string{"../../scripts/installed-cycle.sh", "../../scripts/installed-cycle-set.sh"} {
		b, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		if p := cleanupProblems(string(b)); len(p) != 0 {
			t.Errorf("%s: %v", script, p)
		}
	}
	const good = "root=\"\" n=0 dir=\"$(mktemp -d)\"\n# cleanup's inputs: root n dir\ncleanup() {\n  local p\n  ((n)) && rm -rf \"$root\" \"$dir\"\n}\ntrap cleanup EXIT\n"
	if p := cleanupProblems(good); len(p) != 0 {
		t.Errorf("a cleanup of its own variables: %v", p)
	}
	for name, c := range map[string]struct{ text, want string }{
		"an unlisted read":                   {strings.Replace(good, "\"$dir\"", "\"$dir\" \"$other\"", 1), "cleanup reads [dir n other root]"},
		"a listed name unread":               {strings.Replace(good, "inputs: root n dir", "inputs: root n dir x", 1), "its comment lists [root n dir x]"},
		"no assignment":                      {strings.Replace(good, "root=\"\" ", "", 1), "cleanup reads root, which is not assigned"},
		"an inherited value":                 {strings.Replace(good, "root=\"\"", "root=\"${root:-}\"", 1), "root is assigned \"${root:-}\""},
		"an inherited value after a literal": {strings.Replace(good, "root=\"\"", "root=\"\"${root:-}", 1), "root is assigned \"\"${root:-}"},
		"an indented one alone":              {strings.Replace(strings.Replace(good, "root=\"\" ", "", 1), "# cleanup's", "if true; then\n  root=\"\"\nfi\n# cleanup's", 1), "cleanup reads root, which is not assigned"},
		"an indirect read":                   {strings.Replace(good, "  local p\n", "  local p\n  p=root; echo \"${!p}\"\n", 1), "cleanup reads through \"${!\""},
		"eval":                               {strings.Replace(good, "  local p\n", "  local p\n  eval \"echo $root\"\n", 1), "cleanup reads through \"eval \""},
		"a nameref":                          {strings.Replace(good, "  local p\n", "  local -n p=root\n", 1), "cleanup reads through \"local -n\""},
	} {
		if p := cleanupProblems(c.text); !strings.Contains(strings.Join(p, "; "), c.want) {
			t.Errorf("%s: %v, want %q", name, p, c.want)
		}
	}
}
