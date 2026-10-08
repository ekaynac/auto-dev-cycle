// SPDX-License-Identifier: Apache-2.0

package fakes

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const design11 = "docs/design/m0/11-testing.md"

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no module root at %s: %v", root, err)
	}
	return root
}

// fakeDirs are the repository's fake directories: those named fake*, and
// each directory under Prefix.
func fakeDirs(t *testing.T, root string) []string {
	t.Helper()
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == root {
			return err
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" || name == "testdata" || name == "vendor" {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(name, "fake") && rel+"/" != Prefix || strings.HasPrefix(rel, Prefix) {
			dirs = append(dirs, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

func checkRepository(root string, all []Fake, dirs []string) []string {
	var errs []string
	registered := map[string]bool{}
	for _, f := range all {
		registered[f.Path] = true
		info, err := os.Stat(filepath.Join(root, f.Path))
		switch exists := err == nil && info.IsDir(); {
		case f.Built && !exists:
			errs = append(errs, f.Name+" is registered built, but "+f.Path+" is missing")
		case !f.Built && exists:
			errs = append(errs, f.Name+" is registered unbuilt, but "+f.Path+" exists: mark it built")
		}
	}
	for _, d := range dirs {
		if !registered[d] && !registered[filepath.ToSlash(filepath.Dir(d))] {
			errs = append(errs, d+" is a fake the registry does not list")
		}
	}
	return errs
}

// Every fake directory is registered, and each entry's directory exists
// exactly when it is registered built.
func TestRegistryMatchesRepository(t *testing.T) {
	root := repoRoot(t)
	dirs := fakeDirs(t, root)
	for _, e := range checkRepository(root, All(), dirs) {
		t.Error(e)
	}
	// Red-proofs: an unlisted fake, a built entry gone, an unbuilt one present.
	extra := append(append([]string(nil), dirs...), "internal/other/fakething")
	if len(checkRepository(root, All(), extra)) == 0 {
		t.Error("an unlisted fake directory passed")
	}
	for i, f := range All() {
		flipped := All()
		flipped[i].Built = !f.Built
		if len(checkRepository(root, flipped, dirs)) == 0 {
			t.Errorf("%s registered with Built=%v passed", f.Name, !f.Built)
		}
	}
}

// tableScopes reads 11's fake table: each row's name and M0 scope.
func tableScopes(t *testing.T, root string) map[string]string {
	t.Helper()
	file, err := os.Open(filepath.Join(root, design11))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	rows, scope := map[string]string{}, -1
	for sc := bufio.NewScanner(file); sc.Scan(); {
		line := sc.Text()
		if !strings.HasPrefix(line, "|") {
			if scope >= 0 {
				break
			}
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		switch {
		case scope < 0 && cells[0] == "Fake":
			for i, c := range cells {
				if c == "M0 scope" {
					scope = i
				}
			}
			if scope < 0 {
				t.Fatalf("%s: the fake table has no M0 scope column", design11)
			}
		case scope >= 0 && !strings.HasPrefix(cells[0], "---") && scope < len(cells):
			rows[cells[0]] = cells[scope]
		}
	}
	if len(rows) == 0 {
		t.Fatalf("%s: no fake table found", design11)
	}
	return rows
}

// 11's table and the registry agree: every row is registered with the
// table's M0 scope, and every fake under Prefix is a row.
func TestRegistryMatchesDesign(t *testing.T) {
	rows := tableScopes(t, repoRoot(t))
	byName := map[string]Fake{}
	for _, f := range All() {
		byName[f.Name] = f
		if _, ok := rows[f.Name]; !ok && strings.HasPrefix(f.Path, Prefix) {
			t.Errorf("%s lives under %s but is not a row of %s", f.Name, Prefix, design11)
		}
	}
	for name, scope := range rows {
		f, ok := byName[name]
		switch {
		case !ok:
			t.Errorf("%s: %q is not registered", design11, name)
		case f.M0Scope != scope:
			t.Errorf("%s: %s has M0 scope %q, the registry %q", design11, name, scope, f.M0Scope)
		}
	}
}
