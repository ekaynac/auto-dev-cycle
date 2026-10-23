// SPDX-License-Identifier: Apache-2.0

package contract

import (
	"strings"
	"testing"
)

// Mythos #425 M1: IsDigest is the schema's Digest, read from the embedded
// common/defs: sha256: and 64 lowercase hex, nothing else; DefPattern
// refuses a def with no pattern, printing an error only when one occurred.
func TestIsDigestIsTheSchemas(t *testing.T) {
	good := "sha256:" + strings.Repeat("0a", 32)
	for s, want := range map[string]bool{
		good: true, "sha256:" + strings.Repeat("A", 64): false, "sha256:" + strings.Repeat("a", 63): false,
		"sha256:" + strings.Repeat("a", 65): false, "sha512:" + strings.Repeat("a", 64): false, " " + good: false, good + "\n": false, "": false,
	} {
		if IsDigest(s) != want {
			t.Errorf("IsDigest(%q) = %v", s, !want)
		}
	}
	if DefPattern("common/defs", "Digest").String() != "^sha256:[0-9a-f]{64}$" {
		t.Errorf("the schema's Digest pattern: %s", DefPattern("common/defs", "Digest"))
	}
	for def, want := range map[string]string{"no-such-def": "contract: common/defs's $defs.no-such-def has no pattern", "": "contract: common/defs's $defs. has no pattern"} {
		func() { // the panic names the def, and an error only when one occurred (Mythos #425 r2 L3)
			defer func() {
				if r := recover(); r != want {
					t.Errorf("DefPattern(%q) panicked %v, want %q", def, r, want)
				}
			}()
			DefPattern("common/defs", def)
		}()
	}
	func() {
		defer func() {
			if r, _ := recover().(string); !strings.HasPrefix(r, "contract: no-such-schema's $defs.Digest: ") || strings.Contains(r, "<nil>") {
				t.Errorf("DefPattern of a missing schema panicked %q", r)
			}
		}()
		DefPattern("no-such-schema", "Digest")
	}()
}
