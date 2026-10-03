// SPDX-License-Identifier: Apache-2.0

// Package schemas embeds the schema files: the single source of every
// persisted or exchanged shape (docs/design/m0/00 cross-cutting rule 1). The
// binary validates against exactly these bytes, and SetDigest names them.
package schemas

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"sort"
)

// Base is the $id prefix of every schema; an id is Base + path.
const Base = "https://github.com/ekaynac/auto-dev-cycle/schemas/"

//go:embed */*.json
var files embed.FS

// FS returns the embedded schema tree, rooted at schemas/.
func FS() fs.FS { return files }

// Paths lists the embedded schema files (e.g. "records/receipt.json"), sorted.
func Paths() ([]string, error) {
	paths, err := fs.Glob(files, "*/*.json")
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

// SetDigest is the schema-set digest: sha256 over every schema file's path and the sha256 of its bytes, in path order. Every build and every event names it
// (docs/design/m0/01 "Build and CI", 03 events.schema_set).
func SetDigest() (string, error) {
	paths, err := Paths()
	if err != nil {
		return "", err
	}
	set := sha256.New()
	for _, p := range paths {
		b, err := fs.ReadFile(files, p)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(b)
		set.Write([]byte(p))
		set.Write([]byte{0})
		set.Write([]byte(hex.EncodeToString(sum[:])))
		set.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(set.Sum(nil)), nil
}
