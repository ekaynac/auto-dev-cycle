// SPDX-License-Identifier: Apache-2.0

package contract

import (
	"strings"
	"testing"
)

// Mythos #425 M1: IsDigest is the schema's Digest, read from the embedded
// common/defs: sha256: and 64 lowercase hex, nothing else; DefPattern
// refuses a def with no pattern.
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
	defer func() {
		if recover() == nil {
			t.Error("a def with no pattern did not panic")
		}
	}()
	DefPattern("common/defs", "no-such-def")
}
