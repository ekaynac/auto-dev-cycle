// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The integrity guard (01 §2.5 [A6], P14b) reads changed Go files by
// syntax (comments and strings never count) and blocks, with no approved
// row (M1 has none): a test gone, or no longer surely collected on a gate
// platform; fewer assertions or more skips in a test (tests by package);
// a skip added outside a test where the base has Go tests; a go.mod added
// over tests, or a root go.mod's ignore directives changed; a Go file it
// cannot read whole or parse; a way to forge test2json results (#226
// entry 32). Python is P14c's: until then, a Python change in a base with
// Python tests blocks. It warns the reviewer of every exit, a changed
// TestMain or exiting init, and a 22 converted to a byte or rune. It
// reads at most maxRead bytes of blobs in all, maxFile per file (vars:
// tests lower them).
var (
	maxRead int64 = 64 << 20
	maxFile int64 = 4 << 20
	// pyFiles change what pytest collects or how (P14c reads them).
	pyFiles = []string{"conftest.py", "pytest.ini", ".pytest.ini", "pytest.toml", ".pytest.toml", "tox.ini", "setup.cfg",
		"pyproject.toml", "pyvenv.cfg"}
)

// stat is one test's evidence.
type stat struct{ asserts, skips int }

// facts is what the guard reads of one Go file.
type facts struct {
	tests                              map[string]*stat  // by package and name
	ambient, forged, exits, twentyTwos int               // skips outside tests, forging markers, …
	forge                              bool              // detects test mode and exits at init
	ignore                             string            // a root go.mod's ignore directives
	warn                               map[string]string // TestMain, exiting inits: compared whole
}

// side is one side of a change. on is, per gate platform, 2 when go test
// surely collects it, 1 when that depends on tags the guard does not
// model, 0 when it never does; cons is its build constraint lines; why,
// why it was not read.
type side struct {
	path, why, cons string
	on              []int
	facts           facts
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
	files            []string // the base's, once listed
	huge             bool     // the base's list is over its bound
	blocks, warnings []Block
}

func add(to *[]Block, p, what string) {
	*to = append(*to, Block{vocab.ReasonTestIntegrityViolation, p, what})
}

// side reads a Go file's or root go.mod's blob (when want) within the
// bounds, and what the guard knows of it.
func (r *run) side(ctx context.Context, want bool, p, blob string) (side, error) {
	s := side{path: p, on: make([]int, len(platforms)), facts: facts{tests: map[string]*stat{}, warn: map[string]string{}}}
	if !want || path.Ext(p) != ".go" && p != "go.mod" {
		return s, nil
	}
	if err := ctx.Err(); err != nil {
		return s, err
	}
	b, ok, err := r.read(ctx, blob, min(maxFile, r.budget))
	if err != nil || !ok {
		s.why = "over the guard's read bound"
		return s, err
	}
	r.budget -= int64(len(b))
	if p == "go.mod" {
		s.facts.ignore = goIgnores(b)
	} else if err := goFacts(p, b, &s); err != nil {
		s.why = "not parsed: " + err.Error()
	}
	return s, nil
}

// change applies the per-file rules to one change.
func (r *run) change(ctx context.Context, c Change, src, dst side) error {
	if py := func(p string) bool { return path.Ext(p) == ".py" || slices.Contains(pyFiles, path.Base(p)) }; py(c.Src) || py(c.Dst) {
		if tested, err := r.hasTests(ctx, ".py", ""); err != nil || tested {
			add(&r.blocks, c.Dst, "python integrity not verified (P14c)")
			return err
		}
	}
	if src.why != "" {
		add(&r.blocks, src.path, "unverifiable: "+src.why)
	}
	if dst.why != "" && (src.why == "" || dst.path != src.path) { // an unread file once
		add(&r.blocks, dst.path, "unverifiable: "+dst.why)
	}
	d, s := dst.facts, src.facts
	switch {
	case src.why != "" || dst.why != "":
		return nil
	case s.ignore != d.ignore:
		add(&r.blocks, c.Dst, "a root go.mod's ignore directives changed: packages leave ./...")
	}
	if c.Status == "D" {
		return nil // its tests are compared by name
	}
	for i, pl := range platforms {
		if src.on[i] > 0 && dst.on[i] < 2 && (src.on[i] == 2 || src.cons != dst.cons) {
			add(&r.blocks, c.Dst, fmt.Sprintf("a test file no longer surely collected (%s/%s)", pl[0], pl[1]))
		}
	}
	tested, err := false, error(nil)
	if d.ambient > s.ambient {
		if tested, err = r.hasTests(ctx, ".go", ""); tested || slices.Max(dst.on) > 0 {
			add(&r.blocks, c.Dst, "a skip added outside a test (a helper)")
		}
	}
	if path.Base(c.Dst) == "go.mod" && c.Dst != "go.mod" && c.Status != "M" && err == nil { // added, copied or moved
		if tested, err = r.hasTests(ctx, ".go", path.Dir(c.Dst)+"/"); tested {
			add(&r.blocks, c.Dst, "a go.mod added over tests: they leave the module the gate tests")
		}
	}
	if d.forged > s.forged || d.forge {
		add(&r.blocks, c.Dst, "can forge go test2json results (testing.Testing, a 0x16 byte, or test-mode detection with an exit at init)")
	}
	if d.exits > 0 {
		add(&r.warnings, c.Dst, "exits the process (os.Exit, syscall.Exit, runtime.Goexit): review it")
	}
	if d.twentyTwos > 0 {
		add(&r.warnings, c.Dst, "converts 22 (0x16, test2json's marker) to a byte or rune: review it")
	}
	for _, k := range slices.Sorted(maps.Keys(d.warn)) {
		if d.warn[k] != s.warn[k] {
			add(&r.warnings, c.Dst, k+" changed: review it")
		}
	}
	return err
}

// merge adds a collected side's tests to into.
func merge(into map[string]*stat, s side) {
	for k, t := range s.facts.tests {
		if slices.Max(s.on) > 0 {
			u := cmp.Or(into[k], &stat{})
			u.asserts, u.skips = u.asserts+t.asserts, u.skips+t.skips
			into[k] = u
		}
	}
}

// compare blocks a test gone or weakened.
func (r *run) compare(before, after map[string]*stat) {
	for _, k := range slices.Sorted(maps.Keys(before)) {
		switch b, a := before[k], after[k]; {
		case a == nil:
			add(&r.blocks, k, "a test deleted or no longer collected")
		case a.asserts < b.asserts:
			add(&r.blocks, k, fmt.Sprintf("fewer assertions in a test (%d → %d)", b.asserts, a.asserts))
		case a.skips > b.skips:
			add(&r.blocks, k, "a test skipped")
		}
	}
}

// hasTests: the base holds a test file of ext (".go", ".py") collected by
// its name, under dir ("" for anywhere). A base list over its bound is
// blocked once, as unknown, and has tests everywhere.
func (r *run) hasTests(ctx context.Context, ext, dir string) (bool, error) {
	if r.files == nil {
		g, cleanup, err := newIndex(ctx, r.repo, r.base)
		if err != nil {
			return false, err
		}
		defer cleanup()
		out, err := g.run(ctx, nil, "ls-files", "-z")
		if r.huge = errors.Is(err, ErrTooLarge); r.huge {
			add(&r.blocks, "", "the base's file list is over its bound: where its tests are is unknown")
		} else if err != nil {
			return false, err
		}
		r.files = strings.Split(string(out), "\x00") // never nil: listed once
	}
	return r.huge || slices.ContainsFunc(r.files, func(p string) bool {
		b := path.Base(p)
		return strings.HasPrefix(p, dir) && (ext == ".go" && goByName(p) ||
			ext == ".py" && strings.HasSuffix(b, ".py") && (strings.HasPrefix(b, "test_") || strings.HasSuffix(b, "_test.py")))
	}), nil
}

// walked: no directory of p is one skip names (the runner never walks it).
func walked(p string, skip func(dir string) bool) bool {
	d := path.Dir(p)
	return d == "." || !slices.ContainsFunc(strings.Split(d, "/"), skip)
}
