// SPDX-License-Identifier: Apache-2.0

package accept

import (
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

// The integrity guard (01 §2.5 [A6], P14b) reads each touched package's Go
// files, base and candidate, by syntax and types. It blocks (no approved
// row in M1) a test gone, less surely collected, with fewer assertions or
// more skips (by platform, package and name), a skip outside tests, and
// test2json forgery; what it cannot prove safe is blocked as unverifiable
// (unread or unparsed Go, initialization reaching an exit, output or an
// unresolved call, linkname, unsafe, cgo, assembly, reflection on testing
// values, go.mod changes that hide tests). Python fails closed until P14c;
// table rows, subtests and helper skips are P14d's.
var (
	maxRead  int64 = 64 << 20 // blob bytes in all
	maxFile  int64 = 4 << 20  // per blob
	maxWork        = 1 << 24  // build-constraint tag evaluations in all
	maxReach       = 4096     // functions followed from a package's initialization
	// pyFiles change what pytest collects or how (P14c reads them).
	pyFiles = []string{"conftest.py", "pytest.ini", ".pytest.ini", "pytest.toml", ".pytest.toml", "tox.ini", "setup.cfg",
		"pyproject.toml", "pyvenv.cfg"}
)

// stat is one test's evidence on one platform: cert is 2 when go test
// surely collects its file there, 1 when that depends on tags the guard
// does not model; cons is its file's constraint lines.
type stat struct {
	asserts, skips, cert int
	cons                 string
}

// pkgFacts is what the guard reads of one version of a package directory.
type pkgFacts struct {
	tests                              map[string]*stat    // "platform dir:package.Name"
	ambient, forged, exits, twentyTwos int                 // skips outside tests, forging markers, …
	init                               map[string]int      // what initialization reaches
	warn                               map[string]string   // TestMain(*testing.M), compared whole
	flags                              map[string][]string // per file: linkname, unsafe, …
	bad                                map[string]string   // per file: why it is not verified
}

// Integrity is the integrity guard for a candidate of base in repo, for
// Accept's In.Guard. It lists the base's files through a temporary index.
func Integrity(repo sandbox.Repo, base string) Guard {
	return func(ctx context.Context, changes []Change, read Reader) ([]Block, []Block, error) {
		r := &run{ctx: ctx, repo: repo, base: base, read: read, budget: maxRead, blobs: map[string][]byte{}, touched: map[string]bool{}}
		err := r.check(changes)
		return r.blocks, r.warnings, err
	}
}

type run struct {
	ctx                context.Context
	repo               sandbox.Repo
	base, mod          string // mod: the candidate's module path
	read               Reader
	budget             int64
	work               int
	blobs              map[string][]byte   // blobs read, once
	files, cand        map[string]string   // the base's and the candidate's paths, to blobs
	baseDirs, candDirs map[string][]string // their Go files by directory
	touched            map[string]bool     // directories of changed Go files
	blocks, warnings   []Block
}

func add(to *[]Block, p, what string) {
	*to = append(*to, Block{vocab.ReasonTestIntegrityViolation, p, what})
}

func (r *run) check(changes []Change) error {
	if err := r.list(); err != nil || r.files == nil {
		return err // a base list over its bound is blocked
	}
	r.cand = maps.Clone(r.files)
	for _, c := range changes {
		if c.Status != "A" && c.Status != "C" { // a copy's source is unchanged
			delete(r.cand, c.Src)
		}
		if c.Status != "D" {
			r.cand[c.Dst] = c.DstBlob
		}
	}
	r.baseDirs, r.candDirs = byDir(r.files), byDir(r.cand)
	py := slices.ContainsFunc(slices.Collect(maps.Keys(r.files)), func(p string) bool { return path.Ext(p) == ".py" })
	for _, c := range changes {
		for _, p := range slices.Compact([]string{c.Src, c.Dst}) {
			switch dir, new := path.Dir(p), c.Status != "D" && p == c.Dst; {
			case py && (path.Ext(p) == ".py" || slices.Contains(pyFiles, path.Base(p)) || p == "conda-meta/history" || strings.HasSuffix(p, "/conda-meta/history")):
				add(&r.blocks, p, "python integrity not verified (P14c)")
			case path.Ext(p) == ".go":
				r.touched[dir] = true
			case new && slices.Contains([]string{".s", ".S", ".syso"}, path.Ext(p)) && r.tested(dir):
				add(&r.blocks, p, "unverifiable: assembly or a system object in a package with tests")
			case new && path.Base(p) == "go.mod" && p != "go.mod" && c.Status != "M" && slices.ContainsFunc(slices.Collect(maps.Keys(r.files)),
				func(q string) bool { return goByName(q) && strings.HasPrefix(q, dir+"/") }):
				add(&r.blocks, p, "a go.mod added over tests: they leave the module the gate tests")
			}
		}
	}
	if err := r.goMod(); err != nil {
		return err
	}
	for _, dir := range slices.Sorted(maps.Keys(r.touched)) {
		b, err := r.pkg(dir, r.files, r.baseDirs)
		if err != nil {
			return err
		}
		a, err := r.pkg(dir, r.cand, r.candDirs)
		if err != nil {
			return err
		}
		r.compare(dir, b, a)
	}
	return nil
}

// list reads the base's paths and blobs (ls-files -s), once. A list over
// its bound is the base's size, not the candidate's: blocked, as unknown.
func (r *run) list() error {
	g, cleanup, err := newIndex(r.ctx, r.repo, r.base)
	if err != nil {
		return err
	}
	defer cleanup()
	out, err := g.run(r.ctx, nil, "ls-files", "-s", "-z")
	if errors.Is(err, ErrTooLarge) {
		add(&r.blocks, "", "unverifiable: the base's file list is over its bound")
		return nil
	} else if err != nil {
		return err
	}
	r.files = map[string]string{}
	for _, e := range strings.Split(string(out), "\x00") {
		if meta, p, ok := strings.Cut(e, "\t"); ok && len(strings.Fields(meta)) == 3 {
			r.files[p] = strings.Fields(meta)[1]
		}
	}
	return nil
}

func byDir(files map[string]string) map[string][]string {
	out := map[string][]string{}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		out[path.Dir(p)] = append(out[path.Dir(p)], p)
	}
	return out
}

// tested: the package in dir has a test file, in the base or the candidate.
func (r *run) tested(dir string) bool {
	return slices.ContainsFunc(append(slices.Clone(r.baseDirs[dir]), r.candDirs[dir]...), goByName)
}

// file reads a blob within the bounds, once: why says why not.
func (r *run) file(blob string) ([]byte, string, error) {
	if b, ok := r.blobs[blob]; ok {
		return b, "", nil
	}
	b, ok, err := r.read(r.ctx, blob, min(maxFile, r.budget))
	if err != nil || !ok {
		return nil, "over the guard's read bound", err
	}
	r.budget, r.blobs[blob] = r.budget-int64(len(b)), b
	return b, "", nil
}

// goMod reads the candidate's module path, and blocks a root go.mod whose
// ignore directives (Go 1.25: packages ./... leaves out) changed.
func (r *run) goMod() error {
	var ignores [2]string
	for i, blob := range []string{r.files["go.mod"], r.cand["go.mod"]} {
		if blob == "" || r.files["go.mod"] == r.cand["go.mod"] && i == 0 {
			continue
		}
		b, why, err := r.file(blob)
		if err != nil || why != "" {
			add(&r.blocks, "go.mod", "unverifiable: "+why)
			return err
		}
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Fields(l); len(f) == 2 && f[0] == "module" && i == 1 {
				r.mod = strings.Trim(f[1], `"`)
			}
		}
		ignores[i] = goIgnores(b)
	}
	if r.files["go.mod"] != r.cand["go.mod"] && ignores[0] != ignores[1] {
		add(&r.blocks, "go.mod", "unverifiable: a root go.mod's ignore directives changed (packages leave ./...)")
	}
	return nil
}

// pkg reads one version of the package directory dir.
func (r *run) pkg(dir string, files map[string]string, dirs map[string][]string) (pkgFacts, error) {
	fa := pkgFacts{tests: map[string]*stat{}, init: map[string]int{}, warn: map[string]string{}, flags: map[string][]string{}, bad: map[string]string{}}
	srcs := map[string][]byte{}
	for _, p := range dirs[dir] {
		if path.Ext(p) != ".go" {
			continue
		}
		b, why, err := r.file(files[p])
		if err != nil {
			return fa, err
		} else if why != "" {
			fa.bad[p] = why
		} else {
			srcs[p] = b
		}
	}
	return fa, r.analyse(dir, srcs, &fa)
}

// compare blocks what one package directory lost, or gained, between the
// base (b) and the candidate (a).
func (r *run) compare(dir string, b, a pkgFacts) {
	bad := maps.Clone(b.bad)
	maps.Copy(bad, a.bad)
	for _, p := range slices.Sorted(maps.Keys(bad)) {
		add(&r.blocks, p, "unverifiable: "+bad[p])
	}
	if len(bad) > 0 {
		return // the facts are incomplete
	}
	for _, k := range slices.Sorted(maps.Keys(b.tests)) {
		switch bt, at := b.tests[k], a.tests[k]; {
		case at == nil:
			add(&r.blocks, k, "a test deleted or no longer collected")
		case at.cert < bt.cert || at.cert == 1 && at.cons != bt.cons:
			add(&r.blocks, k, "a test no longer surely collected (its build constraints)")
		case at.asserts < bt.asserts:
			add(&r.blocks, k, fmt.Sprintf("fewer assertions in a test (%d → %d)", bt.asserts, at.asserts))
		case at.skips > bt.skips:
			add(&r.blocks, k, "a test skipped")
		}
	}
	if a.ambient > b.ambient && slices.ContainsFunc(slices.Collect(maps.Keys(r.files)), goByName) {
		add(&r.blocks, dir, "a skip added outside a test (a helper)")
	}
	if a.forged > b.forged {
		add(&r.blocks, dir, "can forge go test2json results (testing.Testing or a 0x16 byte)")
	}
	for _, k := range slices.Sorted(maps.Keys(a.init)) {
		if a.init[k] > b.init[k] {
			add(&r.blocks, dir, "unverifiable: the package's initialization (tests included) reaches "+k)
		}
	}
	for _, p := range slices.Sorted(maps.Keys(a.flags)) {
		for _, f := range a.flags[p] {
			if !slices.Contains(b.flags[p], f) && (r.tested(dir) || f == "reflection on a testing value") {
				add(&r.blocks, p, "unverifiable: "+f)
			}
		}
	}
	for _, k := range slices.Compact(slices.Sorted(slices.Values(slices.Concat(slices.Collect(maps.Keys(b.warn)), slices.Collect(maps.Keys(a.warn)))))) {
		if a.warn[k] != b.warn[k] {
			add(&r.warnings, dir, k+" changed or deleted: review it")
		}
	}
	if a.exits != b.exits {
		add(&r.warnings, dir, "its exits changed (os.Exit, syscall.Exit, log.Fatal, …): review them")
	}
	if a.twentyTwos > b.twentyTwos {
		add(&r.warnings, dir, "converts 22 (0x16, test2json's marker) to a byte or rune: review it")
	}
}

// walked: no directory of p is one skip names (the runner never walks it).
func walked(p string, skip func(dir string) bool) bool {
	d := path.Dir(p)
	return d == "." || !slices.ContainsFunc(strings.Split(d, "/"), skip)
}
