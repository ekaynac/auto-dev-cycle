// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"testing"
)

// F (P14e-2b, #226 entries 82 and 85): os.Args handed to a function value,
// another package's variable in a composite literal from initialization,
// each reproduced first on main (326b55e).
// forgeU is a package using util with a Stringer that does nothing.
const forgeU = "package pkg\n\nimport \"example.com/m/util\"\n\ntype forger struct{}\n\nfunc (forger) String() string { return \"\" }\n\n"

func TestIntegrityRest(t *testing.T) {
	u := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"util/u.go": utilGo})
		plain(t, src, "add", "--all")
	})
	for name, c := range map[string]guardCase{
		// 82 (c): os.Args to a function value, outside initialization and the hook.
		"82c os.Args to a function value": {map[string]string{"pkg/args.go": argsGo + "var set = func(a []string) { a[0] = \"-h\" }\n\nfunc reset() { set(os.Args) }\n"},
			nil, "pkg/args.go: os.Args assigned in reset"},
		// 82 (d): another package's container in a composite literal, read from initialization.
		"82d a composite literal of util.S": {map[string]string{"pkg/forge.go": forgeU + "type holder struct{ s []any }\n\nfunc init() {\n\th := holder{util.S}\n\th.s[0] = forger{}\n}\n"},
			nil, "plain data read"},
		"82d a keyed, addressed literal": {map[string]string{"pkg/forge.go": forgeU + "type holder struct{ s []any }\n\nfunc init() {\n\th := &holder{s: util.S}\n\th.s[0] = forger{}\n}\n"},
			nil, "plain data read"},
	} {
		v := u.guarded(t, c.files, c.gone...)
		if !finds(v.Blocks, c.why) {
			t.Errorf("%s: %q", name, whats(v.Blocks))
		}
	}
}
