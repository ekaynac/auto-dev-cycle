// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The integrity guard (01 §2.5 [A6], P14b) reads changed Go and Python
// files by syntax (comments and strings never count) and blocks, with no
// approved row (M1 has none): a test gone or no longer collected on a gate
// platform; fewer assertions, more skips or a literal parametrize row gone
// in a test (Go tests by package, Python tests by file and class); a skip
// added outside a test in a language the base tests; a conftest.py
// collection control, a pytest configuration change, a go.mod added over
// tests; a collected test source it cannot read whole or parse; a Go file
// gaining a way to forge test2json results (#226 entry 32). It warns the
// reviewer of a changed TestMain, exiting init, exit or pytest hook, and
// of an assertion that cannot fail. It reads at most maxRead bytes of
// blobs in all, maxFile per file (vars: tests lower them).
var (
	maxRead int64 = 64 << 20
	maxFile int64 = 4 << 20
	header        = regexp.MustCompile(`^\[\[?\s*[\w.:-]+\s*\]\]?$`) // a plain INI or TOML header
)

// stat is one test's evidence; local, a Python test's name in its file.
type stat struct {
	asserts, skips int
	rows           []string
	local          string
}

// facts is what the guard reads of one file.
type facts struct {
	tests                              map[string]*stat  // by qualified name
	ambient, controls, trivial, forged int               // skips outside tests, conftest controls, …
	conf                               string            // its pytest configuration
	warn                               map[string]string // TestMain, exits, hooks: compared whole
}

// side is one side of a change: on, what collects it; why, why not read.
type side struct {
	path, why string
	on        []string
	facts     facts
}

// Integrity is the integrity guard for a candidate of base in repo, for
// Accept's In.Guard. It lists the base's files (a temporary index) only
// when a rule asks whether the base has tests.
func Integrity(repo sandbox.Repo, base string) Guard {
	return func(ctx context.Context, changes []Change, read Reader) ([]Block, []Block, error) {
		r := &run{repo: repo, base: base, read: read, budget: maxRead}
		before, after := map[string]*stat{}, map[string]*stat{}
		for _, c := range changes {
			src, err := r.side(ctx, c.Status != "A" && c.Status != "C", c.Src, c.SrcBlob) // a copy's source is unchanged
			dst, err2 := r.side(ctx, c.Status != "D", c.Dst, c.DstBlob)
			if err = cmp.Or(err, err2); err == nil {
				err = r.change(ctx, c, src, dst)
			}
			if err != nil {
				return nil, nil, err
			}
			merge(before, src)
			merge(after, dst)
		}
		r.compare(before, after)
		return r.blocks, r.warnings, nil
	}
}

type run struct {
	repo             sandbox.Repo
	base             string
	read             Reader
	budget           int64
	files            []string
	blocks, warnings []Block
}

func add(to *[]Block, p, what string) {
	*to = append(*to, Block{vocab.ReasonTestIntegrityViolation, p, what})
}

// side reads p's blob (when want) within the bounds, and what the guard
// knows of it.
func (r *run) side(ctx context.Context, want bool, p, blob string) (side, error) {
	s := side{path: p, facts: facts{tests: map[string]*stat{}, warn: map[string]string{}}}
	if _, conf := pytestConf(p, nil); !want || !conf && path.Ext(p) != ".go" && path.Ext(p) != ".py" {
		return s, nil
	}
	b, ok, err := r.read(ctx, blob, min(maxFile, r.budget))
	if err != nil || !ok {
		s.why = "over the guard's read bound"
		return s, err
	}
	r.budget -= int64(len(b))
	s.facts.conf, _ = pytestConf(p, b)
	switch path.Ext(p) {
	case ".go":
		s.on, err = goOn(p, b), goFacts(p, b, &s.facts)
	case ".py":
		s.on, err = pyOn(p), pyFacts(p, b, &s.facts)
	}
	if err != nil {
		s.why = "not parsed: " + err.Error()
	}
	return s, nil
}

// change applies the per-file rules to one change.
func (r *run) change(ctx context.Context, c Change, src, dst side) error {
	for _, s := range []side{src, dst} {
		if _, conf := pytestConf(s.path, nil); s.why != "" && (conf || goByName(s.path) || pyOn(s.path) != nil || path.Base(s.path) == "conftest.py") {
			add(&r.blocks, s.path, "unverifiable: "+s.why)
		} else if s.why != "" {
			add(&r.warnings, s.path, "not read by the integrity guard ("+s.why+"): review it")
		}
	}
	d, s := dst.facts, src.facts
	switch {
	case src.why != "" || dst.why != "":
		return nil
	case s.conf != d.conf:
		add(&r.blocks, c.Dst, "a pytest configuration changed (collection, options or plugins)")
	}
	if c.Status == "D" {
		return nil // its tests are compared by name
	}
	for _, on := range src.on {
		if !slices.Contains(dst.on, on) {
			add(&r.blocks, c.Dst, fmt.Sprintf("a test file no longer collected (%s)", on))
		}
	}
	tested, err := false, error(nil)
	if d.ambient > s.ambient {
		tested, err = r.hasTests(ctx, path.Ext(c.Dst), "")
		if tested || len(dst.on) > 0 {
			add(&r.blocks, c.Dst, "a skip added outside a test (a helper, a module or class level mark)")
		}
	}
	if path.Base(c.Dst) == "go.mod" && c.Dst != "go.mod" && c.Status != "M" && err == nil { // added, copied or moved
		if tested, err = r.hasTests(ctx, ".go", path.Dir(c.Dst)+"/"); tested {
			add(&r.blocks, c.Dst, "a go.mod added over tests: they leave the module the gate tests")
		}
	}
	if d.forged > s.forged {
		add(&r.blocks, c.Dst, "can forge go test2json results (testing.Testing or a 0x16 byte)")
	}
	if d.controls > 0 && (path.Base(c.Dst) == "conftest.py" || d.controls > s.controls) { // a plugin's hooks too
		add(&r.blocks, c.Dst, "a collection control (collect_ignore, a collection hook) in a conftest.py or a plugin")
	}
	for _, k := range slices.Sorted(maps.Keys(d.warn)) {
		if d.warn[k] != s.warn[k] {
			add(&r.warnings, c.Dst, k+" changed: review it")
		}
	}
	if d.trivial > s.trivial {
		add(&r.warnings, c.Dst, "an assertion that cannot fail added")
	}
	return err
}

// merge adds a collected side's tests to into.
func merge(into map[string]*stat, s side) {
	for k, t := range s.facts.tests {
		if len(s.on) > 0 {
			u := cmp.Or(into[k], &stat{local: t.local})
			u.asserts, u.skips, u.rows = u.asserts+t.asserts, u.skips+t.skips, append(u.rows, t.rows...)
			into[k] = u
		}
	}
}

// compare blocks a test gone or weakened. A Python test gone from one file
// may reappear, by its name within the file, as a new test of another.
func (r *run) compare(before, after map[string]*stat) {
	moved := map[string][]string{}
	for _, k := range slices.Sorted(maps.Keys(after)) {
		if a := after[k]; before[k] == nil && a.local != "" {
			moved[a.local] = append(moved[a.local], k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(before)) {
		b, a := before[k], after[k]
		if m := moved[b.local]; a == nil && b.local != "" && len(m) > 0 {
			a, moved[b.local] = after[m[0]], m[1:]
		}
		switch {
		case a == nil:
			add(&r.blocks, k, "a test deleted or no longer collected")
		case a.asserts < b.asserts:
			add(&r.blocks, k, fmt.Sprintf("fewer assertions in a test (%d → %d)", b.asserts, a.asserts))
		case a.skips > b.skips:
			add(&r.blocks, k, "a test skipped")
		case lost(b.rows, a.rows):
			add(&r.blocks, k, "a parametrize case removed")
		}
	}
}

// lost: a row of b is not in a (as multisets).
func lost(b, a []string) bool {
	left := map[string]int{}
	for _, row := range a {
		left[row]++
	}
	for _, row := range b {
		if left[row]--; left[row] < 0 {
			return true
		}
	}
	return false
}

// hasTests: the base holds a test file of ext (".go", ".py") collected by
// its name, under dir ("" for anywhere).
func (r *run) hasTests(ctx context.Context, ext, dir string) (bool, error) {
	if r.files == nil && (ext == ".go" || ext == ".py") { // listed once: Split is never nil
		g, cleanup, err := newIndex(ctx, r.repo, r.base)
		if err != nil {
			return false, err
		}
		defer cleanup()
		out, err := g.run(ctx, nil, "ls-files", "-z")
		if err != nil {
			return false, err
		}
		r.files = strings.Split(string(out), "\x00")
	}
	return slices.ContainsFunc(r.files, func(p string) bool {
		return strings.HasPrefix(p, dir) && (ext == ".go" && goByName(p) || ext == ".py" && pyOn(p) != nil)
	}), nil
}

// pytestConf is p's pytest configuration, and whether p is a file pytest
// reads one from: all of a pytest.ini, the pytest sections of the others
// (a header naming pytest opens one; a plain header closes it).
func pytestConf(p string, b []byte) (string, bool) {
	switch path.Base(p) {
	case "pytest.ini", ".pytest.ini":
		return string(b), true
	case "tox.ini", "setup.cfg", "pyproject.toml":
	default:
		return "", false
	}
	var out []string
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "[") && strings.Contains(strings.ToLower(t), "pytest") {
			in = true
		} else if header.MatchString(t) {
			in = false
		}
		if in {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n"), true
}

// walked: no directory of p is one skip names (the runner never walks it).
func walked(p string, skip func(dir string) bool) bool {
	d := path.Dir(p)
	return d == "." || !slices.ContainsFunc(strings.Split(d, "/"), skip)
}
