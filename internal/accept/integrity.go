// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The integrity guard (01 §2.5 [A6], P14b) reads each touched package's Go
// files, base and candidate, by syntax and types. It blocks a test lost or
// weakened, a skip outside tests and test2json forgery's known forms; what
// it cannot prove safe is unverifiable. Initialization and the TestMain hook
// may reach only inert code and plain data; references into S block as
// exits in tested packages and otherwise warn naming file and function
// (decision A; P14b-out widens S). Python: integrity_pyguard.go blocks
// forgery (P14c-2a); every changed .py file still fails closed until
// P14c-2b compares test identities.
var (
	maxRead    int64 = 64 << 20 // blob bytes in all
	maxFile    int64 = 4 << 20  // per blob
	maxCgoRead       = maxRead  // the cgo pass's own bound (Go files' imports)
	maxWork          = 1 << 24  // build-constraint tag evaluations and Python tokens in all
	maxReach         = 4096     // functions followed from a package's initialization
	// pyFiles change what pytest collects or how (P14c reads them).
	pyFiles = []string{"conftest.py", "pytest.ini", ".pytest.ini", "pytest.toml", ".pytest.toml", "tox.ini", "setup.cfg",
		"pyproject.toml", "pyvenv.cfg"}
)

// stat is one test's evidence on one platform (cert: goOn's).
type stat struct {
	asserts, skips, cert int
	cons                 string
}

// pkgFacts is what the guard reads of one version of a package directory.
type pkgFacts struct {
	tests                       map[string]*stat    // "platform dir:package.Name"
	ambient, forged, twentyTwos []int               // per platform: skips outside tests, forging markers, …
	refs                        map[string]int      // "file\x00reference into S\x00function"
	init                        map[string]int      // what initialization reaches
	warn                        map[string]string   // TestMain(*testing.M), compared whole
	flags                       map[string][]string // per file: linkname, unsafe, …
	bad                         map[string]string   // per file: why it is not verified
	untyped                     map[string]string   // per package: why its real types are not read (P14e)
	typed                       map[string]int      // "file\x00function\x00fact" the real types show (P14e)
	imports                     map[string]bool     // the package's import paths, its files' together
	bodies                      map[string]string   // "file\x00function\x00platform": the body's hash
	usesS                       map[string][]string // the same: the S packages it references
	tree                        *tree               // the version read
}

func (fa *pkgFacts) uses(key, pkg string) {
	if sPkg(pkg) && !slices.Contains(fa.usesS[key], pkg) || pkg == "" {
		fa.usesS[key] = append(fa.usesS[key], pkg)
	}
}

// Integrity is the integrity guard for a candidate of base in repo, for
// Accept's In.Guard. It lists the base's files through a temporary index,
// and imports' types through list (nil: every Go package unverifiable).
func Integrity(repo sandbox.Repo, base string, list Lister) Guard {
	return func(ctx context.Context, changes []Change, read Reader) (blocks, warnings []Block, err error) {
		r := &run{ctx: ctx, repo: repo, base: base, read: read, lister: list, budget: maxRead, blobs: map[string][]byte{}, cgoBlobs: map[string][]byte{}, touched: map[string]bool{},
			started: time.Now(), listEnd: time.Now().Add(maxListTime)}
		defer func() { // fail closed: a guard fault blocks, never crashes the controller; a done context stops it
			if p := recover(); p != nil && ctx.Err() != nil {
				blocks, warnings, err = append(r.blocks, r.outBlocks...), append(r.warnings, r.outWarnings...), ctx.Err()
			} else if p != nil {
				blocks, warnings, err = append(append(r.blocks, r.outBlocks...), Block{vocab.ReasonTestIntegrityViolation, "", fmt.Sprintf("unverifiable: the guard failed (%v)", p), false}), append(r.warnings, r.outWarnings...), nil
			}
		}()
		err = r.check(changes)
		return append(r.blocks, r.outBlocks...), append(r.warnings, r.outWarnings...), err
	}
}

type run struct {
	ctx                context.Context
	repo               sandbox.Repo
	base               string
	read               Reader
	lister             Lister
	started, listEnd   time.Time // every go list run and type check ends by listEnd
	listRuns           int
	budget             int64
	work               int
	blobs              map[string][]byte   // blobs read, once
	cgoBlobs           map[string][]byte   // read by the cgo pass, not yet charged to the budget
	files, cand        map[string]string   // the base's and the candidate's paths, to blobs
	baseDirs, candDirs map[string][]string // their files by directory
	touched            map[string]bool     // directories of changed Go files
	trees              [2]*tree            // the base and the candidate
	vendorChanged      map[string]bool     // vendor/ directories with a file changed
	anyTest            bool                // a Go test in either version (a helper anywhere the tests may call)
	blocks, warnings   []Block
	outBlocks          []Block // P14b-out's findings: Additive, reported after the base guard's (Astra #276)
	outWarnings        []Block
}

// cgoSources is what cgo compiles beside a package's Go files, and objects
// what go assembles or links (go/build's names; .o, .a and .so through a
// preamble's LDFLAGS): they block in any repository. With cgo anywhere (SWIG
// turns it on by itself), #cgo -I and ${SRCDIR} reach any path, a README or
// NOTES.txt included: only Go files may change.
var (
	cgoSources = strings.Fields(".c .cc .cxx .cpp .m .h .hh .hpp .hxx .f .F .for .f90 .swig .swigcxx")
	objects    = strings.Fields(".s .S .sx .syso .o .a .so")
)

// usesCgo: a SWIG file, or a Go file importing C, anywhere in either version;
// the Go files read with their own bound, not the guard's read budget (one
// not read, past it, or not parsed counts).
func (r *run) usesCgo() bool {
	fset, budget, seen := token.NewFileSet(), maxCgoRead, map[string]bool{}
	for _, q := range keys(r.files, r.cand) {
		if path.Ext(q) == ".swig" || path.Ext(q) == ".swigcxx" {
			return true
		}
		for _, blob := range slices.Compact([]string{r.files[q], r.cand[q]}) {
			if path.Ext(q) != ".go" || blob == "" || seen[blob] { // a blob once: copies share one
				continue
			}
			seen[blob] = true
			b, ok := r.blobs[blob]
			if !ok {
				var err error
				if b, ok, err = r.read(r.ctx, blob, min(maxFile, budget)); err != nil || !ok {
					return true
				}
				budget, r.cgoBlobs[blob] = budget-int64(len(b)), b // read once: file takes it, charged then
			}
			f, err := parser.ParseFile(fset, q, b, parser.ImportsOnly)
			if err != nil || slices.ContainsFunc(f.Imports, func(is *ast.ImportSpec) bool { return is.Path.Value == `"C"` }) {
				return true
			}
		}
	}
	return false
}

func add(to *[]Block, p, what string) {
	*to = append(*to, Block{vocab.ReasonTestIntegrityViolation, p, what, false})
}

// addOut adds a finding P14b-out added: Additive in the report.
func addOut(to *[]Block, p, what string) {
	*to = append(*to, Block{vocab.ReasonTestIntegrityViolation, p, what, true})
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
	r.trees = [2]*tree{{r.files, r.baseDirs, map[string]*typer{}, vendors(keys(r.files))}, {r.cand, r.candDirs, map[string]*typer{}, vendors(keys(r.cand))}}
	r.anyTest = slices.ContainsFunc(keys(r.files, r.cand), goByName)
	r.vendorChanged = vendors(slices.DeleteFunc(keys(r.files, r.cand), func(p string) bool { return r.files[p] == r.cand[p] }))
	py := slices.ContainsFunc(slices.Collect(maps.Keys(r.files)), func(p string) bool { return path.Ext(p) == ".py" })
	cgo := slices.ContainsFunc(changes, func(c Change) bool { return path.Ext(c.Src) != ".go" || path.Ext(c.Dst) != ".go" }) && r.usesCgo()
	for _, c := range changes {
		for _, p := range slices.Compact([]string{c.Src, c.Dst}) {
			switch dir, new := path.Dir(p), c.Status != "D" && p == c.Dst; {
			case py && (path.Ext(p) == ".py" || slices.Contains(pyFiles, path.Base(p)) || p == "conda-meta/history" || strings.HasSuffix(p, "/conda-meta/history")):
				add(&r.blocks, p, "python integrity not verified (P14c)")
			case path.Ext(p) == ".go":
				r.touched[dir] = true
			case strings.HasSuffix(p, ".work") || strings.HasSuffix(p, ".work.sum"): // a workspace (GOWORK may name any) redirects imports
				add(&r.blocks, p, "unverifiable: a workspace file changed (go.work, or one GOWORK may name)")
			case slices.Contains(objects, path.Ext(p)): // assembled or linked into any test binary importing it; deleted too
				add(&r.blocks, p, "unverifiable: assembly or an object file")
			case cgo: // a constructor runs before any test; deleted too (a header found elsewhere)
				add(&r.blocks, p, "unverifiable: a non-Go file in a repository that uses cgo")
			case slices.Contains(cgoSources, path.Ext(p)): // no cgo yet: SWIG turns it on, a C file is refused
				add(&r.blocks, p, "unverifiable: C, C++, Objective-C, Fortran or SWIG source")
			case new && path.Base(p) == "go.mod" && p != "go.mod" && c.Status != "M" && slices.ContainsFunc(slices.Collect(maps.Keys(r.files)),
				func(q string) bool { return goByName(q) && strings.HasPrefix(q, dir+"/") }):
				add(&r.blocks, p, "a go.mod added over tests: they leave the module the gate tests")
			}
		}
	}
	if err := r.goMod(); err != nil {
		return err
	} else if py {
		if err := r.python(changes); err != nil {
			return err
		}
	}
	for _, dir := range slices.Sorted(maps.Keys(r.touched)) {
		b, err := r.pkg(dir, r.trees[0])
		a, err2 := r.pkg(dir, r.trees[1])
		if err = cmp.Or(err, err2); err != nil {
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
	} else if b, ok := r.cgoBlobs[blob]; ok && int64(len(b)) <= min(maxFile, r.budget) {
		r.budget, r.blobs[blob] = r.budget-int64(len(b)), b
		return b, "", nil
	}
	b, ok, err := r.read(r.ctx, blob, min(maxFile, r.budget))
	if err != nil || !ok {
		return nil, "over the guard's read bound", err
	}
	r.budget, r.blobs[blob] = r.budget-int64(len(b)), b
	return b, "", nil
}

// goMod blocks a root go.mod whose ignore directives (Go 1.25: packages
// ./... leaves out) changed.
func (r *run) goMod() error {
	if r.files["go.mod"] == r.cand["go.mod"] {
		return nil
	}
	var ignores [2]string
	for i, blob := range []string{r.files["go.mod"], r.cand["go.mod"]} {
		if b, why, err := r.file(blob); blob != "" && (err != nil || why != "") {
			add(&r.blocks, "go.mod", "unverifiable: "+why)
			return err
		} else if blob != "" {
			ignores[i] = goIgnores(b)
		}
	}
	if ignores[0] != ignores[1] {
		add(&r.blocks, "go.mod", "unverifiable: a root go.mod's ignore directives changed (packages leave ./...)")
	}
	if r.trees[0].typer(r, ".", platforms[0]).mod != r.trees[1].typer(r, ".", platforms[0]).mod { // which imports are own changes
		addOut(&r.outBlocks, "go.mod", "unverifiable: the module path changed")
	}
	return nil
}

// pkg reads one version of the package directory dir.
func (r *run) pkg(dir string, t *tree) (pkgFacts, error) {
	fa := pkgFacts{ambient: make([]int, len(platforms)), forged: make([]int, len(platforms)), twentyTwos: make([]int, len(platforms)),
		tests: map[string]*stat{}, refs: map[string]int{}, init: map[string]int{}, warn: map[string]string{},
		flags: map[string][]string{}, bad: map[string]string{}, untyped: map[string]string{}, typed: map[string]int{}, imports: map[string]bool{}, bodies: map[string]string{}, usesS: map[string][]string{}, tree: t}
	srcs := map[string][]byte{}
	for _, p := range t.dirs[dir] {
		if path.Ext(p) != ".go" {
			continue
		}
		b, why, err := r.file(t.files[p])
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

// keys is the sorted union of maps' keys.
func keys[V any](ms ...map[string]V) []string {
	var out []string
	for _, m := range ms {
		out = append(out, slices.Collect(maps.Keys(m))...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// compare blocks what one package directory lost, or gained, between the
// base (b) and the candidate (a), and warns of what the reviewer must see.
func (r *run) compare(dir string, b, a pkgFacts) {
	seen := map[string]bool{} // keys per platform: each finding reported once
	add := func(to *[]Block, p, what string) {
		if k := p + "\x00" + what; !seen[k] {
			seen[k] = true
			if to == &r.outBlocks || to == &r.outWarnings {
				addOut(to, p, what)
			} else {
				add(to, p, what)
			}
		}
	}
	bad := maps.Clone(b.bad)
	maps.Copy(bad, a.bad)
	for _, p := range slices.Sorted(maps.Keys(bad)) {
		add(&r.blocks, p, "unverifiable: "+bad[p])
	}
	for _, k := range keys(b.untyped, a.untyped) {
		if p, ok := strings.CutPrefix(k, "~"); ok { // P14b-out's walk over its bound
			add(&r.outBlocks, p, "unverifiable: "+cmp.Or(a.untyped[k], b.untyped[k]))
		} else {
			add(&r.blocks, k, "unverifiable: "+cmp.Or(a.untyped[k], b.untyped[k]))
		}
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
	more := func(a, b []int) bool { // on some platform: none is traded across them
		return slices.ContainsFunc(platforms, func(pl [2]string) bool { return a[slices.Index(platforms, pl)] > b[slices.Index(platforms, pl)] })
	}
	if more(a.ambient, b.ambient) && slices.ContainsFunc(slices.Collect(maps.Keys(r.files)), goByName) {
		add(&r.blocks, dir, "a skip added outside a test (a helper)")
	}
	if more(a.forged, b.forged) {
		add(&r.blocks, dir, "can forge go test2json results (testing.Testing, a 0x16 spelled or in a constant, or a testing value not the runner's)")
	}
	for _, k := range slices.Sorted(maps.Keys(a.init)) { // init, flags, warn and refs keys end in "\x00platform"
		if what, _, _ := strings.Cut(k, "\x00"); a.init[k] > b.init[k] {
			add(&r.blocks, dir, "unverifiable: the package's initialization (tests included) reaches "+what)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(a.flags)) {
		p, _, _ := strings.Cut(k, "\x00")
		for _, f := range a.flags[k] {
			cgo := f == "an import of C" && r.files[p] != r.cand[p] // a new or changed preamble runs before any test
			blocks, warnings := &r.blocks, &r.warnings
			if dot, ok := strings.CutPrefix(f, "a dot import of "); ok && !mainS(dot) { // of a package P14b-out put in S
				blocks, warnings = &r.outBlocks, &r.outWarnings
			}
			if (!slices.Contains(b.flags[k], f) || cgo) && (r.tested(dir) || cgo || f == "reflection on a testing value") {
				add(blocks, p, "unverifiable: "+f)
			} else if !slices.Contains(b.flags[k], f) {
				add(warnings, p, f+": review it")
			}
		}
	}
	for _, k := range keys(b.warn, a.warn) {
		if hook, _, _ := strings.Cut(k, "\x00"); a.warn[k] != b.warn[k] {
			add(&r.warnings, dir, hook+" changed or deleted: review it")
		}
	}
	out := map[string][]string{}             // P14b-out's findings: "file\x00what" to functions
	for _, k := range keys(b.refs, a.refs) { // per file and function: added ones block or warn, any other change warns
		f := strings.Split(k, "\x00") // file, reference, function, platform, its package
		pkg := strings.TrimPrefix(f[4], "~")
		additive := !mainS(pkg) || pkg != f[4] // P14b-out's: libc, the new S, every reference by real types
		if at := f[0] + ": " + f[1] + " in " + f[2]; a.refs[k] > b.refs[k] && r.tested(dir) && sensitive(f[1]) == 2 {
			add(map[bool]*[]Block{false: &r.blocks, true: &r.outBlocks}[additive], at, "unverifiable: a new exit, exec, system call or call into testing's runner in a package with tests")
		} else if a.refs[k] != b.refs[k] && additive { // per file and package
			r.work += len(k)
			out[f[0]+"\x00references into "+pkg+" added, removed, moved or changed"] = append(out[f[0]+"\x00references into "+pkg+" added, removed, moved or changed"], f[2])
		} else if a.refs[k] != b.refs[k] {
			add(&r.warnings, at, "a reference into S (exit, output, process, testing control) added, removed, moved or changed: review it")
		}
	}
	for _, k := range keys(a.bodies) { // a body changed, its S references kept (fs.Parse(nil) to fs.Parse([]string{"-h"}))
		if f := strings.Split(k, "\x00"); b.bodies[k] != "" && a.bodies[k] != b.bodies[k] {
			r.work += len(k)
			for _, pkg := range a.usesS[k] {
				out[f[0]+"\x00changed bodies that use "+cmp.Or(pkg, "print")] = append(out[f[0]+"\x00changed bodies that use "+cmp.Or(pkg, "print")], f[1])
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(out)) {
		file, what, _ := strings.Cut(k, "\x00")
		fns := slices.Compact(slices.Sorted(slices.Values(out[k])))
		for i, fn := range fns {
			fns[i] = bound(fn) // a name cut at its source (Astra #276 r2 H4)
		}
		add(&r.outWarnings, file, fmt.Sprintf("%s (%d: %s%s): review it", what, len(fns), strings.Join(fns[:min(3, len(fns))], ", "), map[bool]string{true: ", …"}[len(fns) > 3]))
	}
	own := func(p string) bool { // as the loader resolves it, in both versions: an own package, not vendored nor a nested module's
		return !slices.ContainsFunc([]*tree{b.tree, a.tree}, func(t *tree) bool {
			ty := t.typer(r, dir, platforms[0])
			_, ok := ty.local(p)
			return !ok || ty.vendor[p]
		})
	}
	for _, p := range slices.Sorted(maps.Keys(a.imports)) { // a new edge outside the standard library (go list's) and the module warns (decision A)
		if !b.imports[p] && p != "C" && p != "unsafe" && !a.tree.typer(r, dir, platforms[0]).listed[p].Standard && !own(p) {
			add(&r.outWarnings, dir, "a new import of "+p+", outside the standard library and the module: review it")
		}
	}
	for _, k := range keys(b.typed, a.typed) { // by identity, so no fact is traded for another
		f := strings.Split(k, "\x00") // file, function, fact
		if at := f[0] + ": " + f[2] + " in " + f[1]; a.typed[k] > b.typed[k] && (r.tested(dir) || r.anyTest || !strings.HasPrefix(f[2], factMakes)) {
			add(&r.blocks, at, "can forge go test2json results (real types)")
		} else if a.typed[k] > b.typed[k] {
			add(&r.warnings, at, "reflection that makes values, in a package without tests: review it")
		}
	}
	if more(a.twentyTwos, b.twentyTwos) {
		add(&r.warnings, dir, "converts 22 (0x16, test2json's marker) to a byte or rune: review it")
	}
}

// walked: no directory of p is one skip names (the runner never walks it).
func walked(p string, skip func(dir string) bool) bool {
	d := path.Dir(p)
	return d == "." || !slices.ContainsFunc(strings.Split(d, "/"), skip)
}
