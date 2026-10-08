// SPDX-License-Identifier: Apache-2.0

package fakes

import (
	"bufio"
	"fmt"
	"go/doc"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

var fakeWord = regexp.MustCompile(`(?i)\bfakes?\b`)

// fakeDirs are the repository's fakes, found three ways: a directory named
// fake*, a directory under Prefix, and a Go package whose synopsis (the
// first sentence of its package comment) calls it a fake.
func fakeDirs(t *testing.T, root string) []string {
	t.Helper()
	var dirs []string
	add := func(rel string) { // the registry's own package is not a fake
		if rel+"/" != Prefix && !slices.Contains(dirs, rel) {
			dirs = append(dirs, rel)
		}
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		name := d.Name()
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "testdata" || name == "vendor" {
				return filepath.SkipDir
			}
			if self := filepath.ToSlash(filepath.Join(rel, name)); strings.HasPrefix(name, "fake") || strings.HasPrefix(self, Prefix) {
				add(self)
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly|parser.ParseComments)
		if err != nil {
			return err
		}
		if f.Doc != nil && fakeWord.MatchString(new(doc.Package).Synopsis(f.Doc.Text())) {
			add(rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

// notFakes are directories discovery finds that are parts of a registered
// fake, not fakes of their own. A fake nested in a registered one is not
// hidden by its parent: it must be registered or listed here.
var notFakes = []string{"adapters/fake/src"} // the fake adapter's sources

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
		if f.Built != strings.HasPrefix(f.M0Scope, "built") {
			errs = append(errs, fmt.Sprintf("%s: Built is %v but its M0 scope is %q", f.Name, f.Built, f.M0Scope))
		}
	}
	for _, d := range dirs {
		if !registered[d] && !slices.Contains(notFakes, d) {
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
	for _, want := range []string{"adapters/fake", "internal/capacity/fakesched", Prefix + "clock"} {
		if !slices.Contains(dirs, want) {
			t.Errorf("discovery misses %s (found %v)", want, dirs)
		}
	}
	for _, e := range checkRepository(root, All(), dirs) {
		t.Error(e)
	}
	// Red-proofs: an unlisted fake, a built entry gone, an unbuilt one
	// present, a scope that contradicts Built.
	for _, d := range []string{"internal/other/thing", Prefix + "clock/sub/deeper", "internal/capacity/fakesched/fakething", "adapters/fake/src/fakeprovider"} {
		extra := append(append([]string(nil), dirs...), d)
		if len(checkRepository(root, All(), extra)) == 0 {
			t.Errorf("the unlisted fake directory %s passed", d)
		}
	}
	// An unbuilt entry, consistently scoped, whose directory exists.
	present := []Fake{{"Fake clock", Prefix + "clock", false, "not yet"}}
	if len(checkRepository(root, present, nil)) == 0 {
		t.Error("an unbuilt entry whose directory exists passed")
	}
	for i, f := range All() {
		flipped := All()
		flipped[i].Built = !f.Built
		if len(checkRepository(root, flipped, dirs)) == 0 {
			t.Errorf("%s registered with Built=%v passed", f.Name, !f.Built)
		}
	}
}

// outsideFakes lists the entries of parts that no registered, built fake
// contains.
func outsideFakes(parts []string, all []Fake) []string {
	var out []string
	for _, d := range parts {
		inside := false
		for _, f := range all {
			inside = inside || f.Built && strings.HasPrefix(d, f.Path+"/")
		}
		if !inside {
			out = append(out, d)
		}
	}
	return out
}

// Each notFakes entry lies inside a registered, built fake.
func TestNotFakesAreInsideFakes(t *testing.T) {
	if out := outsideFakes(notFakes, All()); len(out) != 0 {
		t.Errorf("%v are listed as parts of a fake, but no registered fake contains them", out)
	}
	// Red-proofs: a stray entry, a sibling sharing a prefix, an unbuilt fake.
	for _, d := range []string{"internal/other/thing", "adapters/fakesrc", Prefix + "forge/x"} {
		if len(outsideFakes([]string{d}, All())) == 0 {
			t.Errorf("%s passed as part of a fake", d)
		}
	}
}

// A package that calls itself a fake is discovered whatever its name.
func TestDiscoveryReadsSynopses(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for dir, src := range map[string]string{
		"a/stub":  "// Package stub is a fake mail server for tests.\npackage stub\n",
		"a/real":  "// Package real sends mail. Its tests use a fake.\npackage real\n",
		"a/plain": "package plain\n",
	} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "x.go"), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := fakeDirs(t, root); !slices.Equal(got, []string{"a/stub"}) {
		t.Errorf("discovered %v, want [a/stub]", got)
	}
}

type row struct{ path, scope string }

var cellsRe = regexp.MustCompile("^`([^`]+)`$")

// parseTable reads 11's fake table: each row's place and M0 scope by name.
// Places spelled "…/fakes/" are under Prefix.
func parseTable(r io.Reader) (map[string]row, error) {
	rows := map[string]row{}
	var header []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "|") {
			if header != nil {
				break
			}
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		switch {
		case header == nil && cells[0] == "Fake":
			header = cells
		case header == nil, strings.HasPrefix(cells[0], "---"):
		case len(cells) != len(header):
			return nil, fmt.Errorf("row %q has %d cells, the header %d", cells[0], len(cells), len(header))
		default:
			if _, dup := rows[cells[0]]; dup {
				return nil, fmt.Errorf("row %q appears twice", cells[0])
			}
			lives, scope := slices.Index(header, "Lives in"), slices.Index(header, "M0 scope")
			if lives < 0 || scope < 0 {
				return nil, fmt.Errorf("the fake table lacks a Lives in or M0 scope column: %v", header)
			}
			m := cellsRe.FindStringSubmatch(cells[lives])
			if m == nil {
				return nil, fmt.Errorf("row %q: Lives in %q is not one code path", cells[0], cells[lives])
			}
			path, _ := strings.CutPrefix(m[1], "…/fakes/")
			if path != m[1] {
				path = Prefix + path
			}
			rows[cells[0]] = row{path, cells[scope]}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no fake table found")
	}
	return rows, nil
}

func checkDesign(rows map[string]row, all []Fake) []string {
	var errs []string
	byName := map[string]Fake{}
	for _, f := range all {
		byName[f.Name] = f
		if _, ok := rows[f.Name]; !ok {
			errs = append(errs, f.Name+" is registered but not a row of the table")
		}
	}
	for name, r := range rows {
		f, ok := byName[name]
		switch {
		case !ok:
			errs = append(errs, name+" is a row but not registered")
		case f.Path != r.path:
			errs = append(errs, fmt.Sprintf("%s lives in %s by the table, %s by the registry", name, r.path, f.Path))
		case f.M0Scope != r.scope:
			errs = append(errs, fmt.Sprintf("%s has M0 scope %q by the table, %q by the registry", name, r.scope, f.M0Scope))
		}
	}
	return errs
}

// 11's table and the registry hold the same fakes, places and M0 scopes.
func TestRegistryMatchesDesign(t *testing.T) {
	file, err := os.Open(filepath.Join(repoRoot(t), design11))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	rows, err := parseTable(file)
	if err != nil {
		t.Fatalf("%s: %v", design11, err)
	}
	for _, e := range checkDesign(rows, All()) {
		t.Errorf("%s: %s", design11, e)
	}
}

// The parser refuses malformed tables, and checkDesign every kind of drift.
func TestDesignDriftIsRefused(t *testing.T) {
	const head = "| Fake | Faithful to | Lives in | M0 scope |\n|---|---|---|---|\n"
	for name, table := range map[string]string{
		"a short row":       head + "| A | x | `a` |\n",
		"a duplicate row":   head + "| A | x | `a` | s |\n| A | y | `a` | s |\n",
		"no scope column":   "| Fake | Faithful to | Lives in |\n|---|---|---|\n| A | x | `a` |\n",
		"no table":          "text\n",
		"a two-path place":  head + "| A | x | `a`, `b` | s |\n",
		"an over-long line": head + "| A | x | `a` | " + strings.Repeat("s", bufio.MaxScanTokenSize) + " |\n",
	} {
		if _, err := parseTable(strings.NewReader(table)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	rows, err := parseTable(strings.NewReader(head + "| Fake clock | x | `…/fakes/clock` | s |\n| B | x | `b` | s |\n"))
	if err != nil || rows["Fake clock"].path != Prefix+"clock" || rows["B"].path != "b" {
		t.Fatalf("parsed %v, %v", rows, err)
	}
	all := []Fake{{"Fake clock", Prefix + "clock", false, "s"}, {"B", "b", false, "s"}}
	if errs := checkDesign(rows, all); len(errs) != 0 {
		t.Fatalf("a matching table: %v", errs)
	}
	for name, mutate := range map[string]func([]Fake) []Fake{
		"a deleted row":    func(f []Fake) []Fake { return append(f, Fake{"C", "c", false, "s"}) },
		"an extra row":     func(f []Fake) []Fake { return f[:1] },
		"a path drift":     func(f []Fake) []Fake { f[1].Path = "b2"; return f },
		"a scope drift":    func(f []Fake) []Fake { f[0].M0Scope = "t"; return f },
		"a renamed fake":   func(f []Fake) []Fake { f[1].Name = "B2"; return f },
		"a prefixed place": func(f []Fake) []Fake { f[0].Path = "clock"; return f },
	} {
		if len(checkDesign(rows, mutate(slices.Clone(all)))) == 0 {
			t.Errorf("%s passed", name)
		}
	}
}
