// SPDX-License-Identifier: Apache-2.0

package schemas_test

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/schemas"
)

var (
	escape   = regexp.MustCompile(`\\.`)
	class    = regexp.MustCompile(`\[[^\]]*\]`)
	unicodey = regexp.MustCompile(`\\[sS]`)
)

// portable reports why a pattern would match differently under Go's RE2 and
// ECMAScript with the u flag (the TypeScript validators), or "".
func portable(p string) string {
	if unicodey.MatchString(p) {
		return `\s and \S are ASCII in RE2 and Unicode in ECMAScript; write the class out ([\t\n\f\r ])`
	}
	if outside := class.ReplaceAllString(escape.ReplaceAllString(p, ""), ""); regexp.MustCompile(`\.`).MatchString(outside) {
		return "an unescaped . excludes different line terminators in RE2 and ECMAScript"
	}
	return ""
}

func patterns(v any, out *[]string) {
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			if s, ok := e.(string); ok && k == "pattern" {
				*out = append(*out, s)
			} else {
				patterns(e, out)
			}
		}
	case []any:
		for _, e := range v {
			patterns(e, out)
		}
	}
}

// Every pattern means the same to both validators.
func TestPatternsArePortable(t *testing.T) {
	paths, err := schemas.Paths()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range paths {
		b, err := fs.ReadFile(schemas.FS(), p)
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		var ps []string
		patterns(doc, &ps)
		for _, pat := range ps {
			n++
			if why := portable(pat); why != "" {
				t.Errorf("%s: %q: %s", p, pat, why)
			}
		}
	}
	if n == 0 {
		t.Fatal("no patterns found")
	}
	for pat, bad := range map[string]bool{`^\S+$`: true, `^[^@\s]+$`: true, `^a.b$`: true, `^a\.b$`: false, `^[.]+$`: false, `^[^\t\n\f\r ]+$`: false} {
		if (portable(pat) != "") != bad {
			t.Errorf("portable(%q) = %q", pat, portable(pat))
		}
	}
}
