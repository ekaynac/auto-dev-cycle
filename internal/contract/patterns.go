// SPDX-License-Identifier: Apache-2.0

package contract

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"

	"github.com/ekaynac/auto-dev-cycle/schemas"
)

// DefPattern is the pattern of $defs.def in the embedded schema at path
// (without .json), compiled: Go code that checks a value of a schema's
// string type reads its rule from the schema, never a copy of it (Mythos
// #425 M1). It panics on a schema or def with no pattern: a programming
// error, caught at package initialisation.
func DefPattern(path, def string) *regexp.Regexp {
	raw, err := fs.ReadFile(schemas.FS(), path+".json")
	var doc struct {
		Defs map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"$defs"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &doc)
	}
	if err != nil {
		panic(fmt.Sprintf("contract: %s's $defs.%s: %v", path, def, err))
	}
	if doc.Defs[def].Pattern == "" {
		panic(fmt.Sprintf("contract: %s's $defs.%s has no pattern", path, def))
	}
	return regexp.MustCompile(doc.Defs[def].Pattern)
}

// digest is common/defs's Digest: sha256 over the bytes, lowercase hex.
var digest = DefPattern("common/defs", "Digest")

// IsDigest reports whether s is a content digest (common/defs's Digest,
// "sha256:" and 64 lowercase hex): the one Go rule for it.
func IsDigest(s string) bool { return digest.MatchString(s) }
