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
	"strings"
)

// Base is the $id prefix of every schema; an id is Base + path.
const Base = "https://github.com/ekaynac/auto-dev-cycle/schemas/"

// Each schema directory is listed here as it is added.
//
//go:embed common records
var files embed.FS

// FS returns the embedded schema tree, rooted at schemas/.
func FS() fs.FS { return files }

// Paths lists the embedded schema files (e.g. "common/defs.json"), sorted.
func Paths() ([]string, error) { return PathsIn(files) }

// PathsIn lists every .json file in fsys at any depth, sorted.
func PathsIn(fsys fs.FS) ([]string, error) {
	var paths []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json") {
			paths = append(paths, p)
		}
		return err
	})
	sort.Strings(paths)
	return paths, err
}

// SetDigest is the schema-set digest of the embedded files (docs/design/m0/01
// "Build and CI", 03 events.schema_set). Every build and every event names it.
func SetDigest() (string, error) { return DigestOf(files) }

// DigestOf is the schema-set digest of fsys: sha256 over one record per
// schema file, in byte order of path, each record being
//
//	path NUL lowercase-hex(sha256(file bytes)) LF
//
// written "sha256:" + lowercase hex. A path cannot contain NUL or LF, so the
// records are unambiguous.
func DigestOf(fsys fs.FS) (string, error) {
	paths, err := PathsIn(fsys)
	if err != nil {
		return "", err
	}
	set := sha256.New()
	for _, p := range paths {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(b)
		set.Write([]byte(p + "\x00" + hex.EncodeToString(sum[:]) + "\n"))
	}
	return "sha256:" + hex.EncodeToString(set.Sum(nil)), nil
}
