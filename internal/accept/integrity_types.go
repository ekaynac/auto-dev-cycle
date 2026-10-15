// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"go/version"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"golang.org/x/mod/modfile"
)

// The guard's real types (P14e, #226 entries 79, 82, 85), per gate platform:
// own packages type-checked here from the version's blobs, every other import
// from the export data go list writes for that platform beside only go.mod and
// go.sum (never the tree's source). Imports not loaded on any platform, in the
// base or the candidate: unverifiable.
var (
	maxPkgs           = 2000      // own packages one version type-checks
	maxExport   int64 = 256 << 20 // export data bytes one version reads
	maxList     int64 = 16 << 20  // go list's output
	maxListed         = 64        // go list results a LocalLister keeps
	maxListRuns       = 64        // go list runs one guard makes (per platform and module), all within maxListTime
	maxListTime       = 5 * time.Minute
	// failSigs are the failure-setting methods: an interface holding one
	// turns a value asserted to it into an assertion the guard counts.
	failSigs = func() map[string]*types.Signature {
		sig := func(variadic bool, ps ...types.Type) *types.Signature {
			var vs []*types.Var
			for _, p := range ps {
				vs = append(vs, types.NewParam(token.NoPos, nil, "", p))
			}
			return types.NewSignatureType(nil, nil, nil, types.NewTuple(vs...), nil, variadic)
		}
		args := types.NewSlice(types.Universe.Lookup("any").Type())
		return map[string]*types.Signature{"Error": sig(true, args), "Fatal": sig(true, args), "Fail": sig(false), "FailNow": sig(false),
			"Errorf": sig(true, types.Typ[types.String], args), "Fatalf": sig(true, types.Typ[types.String], args)}
	}()
)

// Listed is one package go list reads: its export data file, or its error.
type Listed struct {
	ImportPath, Export string
	Standard           bool
	Error              *struct{ Err string }
}

// Lister lists paths and their dependencies with their export data, for a
// platform, in the module whose go.mod and go.sum are given (nil: none).
type Lister func(ctx context.Context, platform [2]string, gomod, gosum []byte, paths []string) (map[string]Listed, error)

// LocalLister is a Lister running the go command gobin with env added (its
// caches: GOCACHE, GOMODCACHE, HOME), offline: no proxy, no checksum
// database, no workspace, no other toolchain, no cgo, go.mod read-only. A
// run is bounded by timeout, its output by maxList and each process's
// address space by mem bytes (0: unbounded); a cancelled run kills go's
// process group. A go.mod replacing a module by a directory (modfile's
// reading) is refused: only the module cache's source is read. Results are
// kept by go.mod, go.sum and paths. gobin's toolchain (its GOROOT's
// VERSION) must be the guard's own: one release, one export data format.
func LocalLister(gobin string, env []string, timeout time.Duration, mem int64) Lister {
	var mu sync.Mutex
	kept := map[[32]byte]map[string]Listed{}
	version := sync.OnceValue(func() error {
		root, err := filepath.EvalSymlinks(gobin)
		b, err2 := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(root)), "VERSION"))
		if v, _, _ := strings.Cut(string(b), "\n"); cmp.Or(err, err2) == nil && v != runtime.Version() {
			return fmt.Errorf("the toolchain is %s, not the guard's %s", v, runtime.Version())
		}
		return cmp.Or(err, err2)
	})
	return func(ctx context.Context, platform [2]string, gomod, gosum []byte, paths []string) (map[string]Listed, error) {
		if err := version(); err != nil {
			return nil, fmt.Errorf("go list: %w", err)
		}
		h := sha256.New()
		for _, b := range append([][]byte{[]byte(platform[0] + "/" + platform[1]), gomod, gosum}, []byte(strings.Join(paths, "\n"))) {
			_ = binary.Write(h, binary.BigEndian, int64(len(b)))
			h.Write(b)
		}
		key := [32]byte(h.Sum(nil))
		mu.Lock()
		got, ok := kept[key]
		mu.Unlock()
		if ok {
			return got, nil
		}
		got, err := goList(ctx, gobin, env, timeout, mem, platform, gomod, gosum, paths)
		if err == nil {
			mu.Lock()
			if len(kept) >= maxListed {
				clear(kept)
			}
			kept[key] = got
			mu.Unlock()
		}
		return got, err
	}
}

func goList(ctx context.Context, gobin string, env []string, timeout time.Duration, mem int64, platform [2]string, gomod, gosum []byte, paths []string) (map[string]Listed, error) {
	if gomod != nil {
		f, err := modfile.Parse("go.mod", gomod, nil)
		if err != nil {
			return nil, fmt.Errorf("go.mod not parsed: %w", err)
		} else if slices.ContainsFunc(f.Replace, func(r *modfile.Replace) bool { return r.New.Version == "" }) {
			return nil, errors.New("go.mod replaces a module by a directory")
		}
	}
	for _, p := range paths { // no flag, pattern or reserved name
		if p == "" || strings.ContainsAny(p[:1], "-./") || strings.Contains(p, "...") || slices.Contains([]string{"std", "cmd", "all"}, p) ||
			strings.HasPrefix(p, "cmd/") || strings.ContainsFunc(p, func(r rune) bool {
			return r > unicode.MaxASCII || !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("-._~+/", r)
		}) {
			return nil, fmt.Errorf("an import path go list cannot take (%q)", p)
		}
	}
	dir, err := os.MkdirTemp("", "adc-golist-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if gomod == nil {
		gomod = []byte("module adc.invalid/guard\n")
	}
	err = os.WriteFile(filepath.Join(dir, "go.mod"), gomod, 0o600)
	if err == nil && gosum != nil {
		err = os.WriteFile(filepath.Join(dir, "go.sum"), gosum, 0o600)
	}
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	limit := "unlimited"
	if mem > 0 {
		limit = strconv.FormatInt(mem>>10, 10)
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{"-c", `ulimit -v "$1" && shift && exec "$@"`, "sh", limit,
		gobin, "list", "-e", "-export", "-deps", "-json=ImportPath,Export,Error,Standard", "--"}, paths...)...)
	cmd.Dir = dir
	cmd.Env = append(slices.Clone(env), "PATH="+filepath.Dir(gobin)+":/usr/bin:/bin", "GOENV=off", "GOFLAGS=-mod=readonly",
		"GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOTOOLCHAIN=local", "GOTELEMETRY=off", "CGO_ENABLED=0", "GO111MODULE=on",
		"GOOS="+platform[0], "GOARCH="+platform[1]) // last: they win over env's
	out, stderr, err := drain(ctx, cmd, maxList, maxStderr)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("go list: %w", context.Cause(ctx))
	} else if err != nil {
		return nil, fmt.Errorf("go list: %w: %s", err, bytes.TrimSpace(stderr[:min(len(stderr), 512)]))
	}
	got := map[string]Listed{}
	for dec := json.NewDecoder(bytes.NewReader(out)); ; {
		var l Listed
		if err := dec.Decode(&l); errors.Is(err, io.EOF) {
			return got, nil
		} else if err != nil {
			return nil, fmt.Errorf("go list: %w", err)
		}
		got[l.ImportPath] = l
	}
}

// tree is one version of the repository: paths to blobs, by directory, and
// a typer per module root and platform.
type tree struct {
	files   map[string]string
	dirs    map[string][]string
	typers  map[string]*typer
	vendors map[string]bool // its vendor/ directories
}

// vendors is the vendor/ directories ("x/vendor/") the paths are under,
// once per run: per module would be its paths times its modules.
func vendors(paths []string) map[string]bool {
	out := map[string]bool{}
	for _, p := range paths {
		for i := 0; i < len(p); {
			j := strings.Index(p[i:], "vendor/")
			if j < 0 {
				break
			}
			if k := i + j; k == 0 || p[k-1] == '/' {
				out[p[:k+len("vendor/")]] = true
			}
			i += j + 1
		}
	}
	return out
}

// typer is one module's packages in one version: its own type-checked here,
// the rest from export data.
type typer struct {
	r            *run
	t            *tree
	platform     [2]string
	root, mod    string // the module's directory and path ("": no module)
	gomod, gosum []byte
	why          string // why nothing loads (the module unread)
	fset         *token.FileSet
	gc           types.Importer
	export       int64
	listed       map[string]Listed
	parsed       map[string][]*ast.File    // own packages' files, by directory
	pkgs         map[string]*types.Package // own packages, by import path (nil: being checked)
	failed       map[string]string         // import paths not loaded, and why
	self         map[string]*types.Package // analysed packages with their tests, for their external tests only
	vendor       map[string]bool           // the packages vendor/modules.txt lists (nil: not vendored)
}

// typer is the typer of dir's module (the nearest go.mod above it) on pl.
func (t *tree) typer(r *run, dir string, pl [2]string) *typer {
	root := ""
	for d := dir; root == ""; d = path.Dir(d) {
		if t.files[path.Join(d, "go.mod")] != "" {
			root = d
		} else if d == "." {
			break
		}
	}
	if ty := t.typers[root+"\x00"+pl[0]+"/"+pl[1]]; ty != nil {
		return ty
	}
	ty := &typer{r: r, t: t, platform: pl, root: root, fset: token.NewFileSet(), export: maxExport, listed: map[string]Listed{},
		parsed: map[string][]*ast.File{}, pkgs: map[string]*types.Package{}, failed: map[string]string{}, self: map[string]*types.Package{}}
	ty.gc = importer.ForCompiler(ty.fset, "gc", ty.lookup)
	t.typers[root+"\x00"+pl[0]+"/"+pl[1]] = ty
	read := func(name string) []byte {
		blob := t.files[path.Join(root, name)]
		if blob == "" || ty.why != "" {
			return nil
		}
		b, why, err := r.file(blob)
		ty.why = cmp.Or(why, errText(err))
		return b
	}
	if ty.gomod, ty.gosum = read("go.mod"), read("go.sum"); ty.gomod != nil && ty.why == "" {
		f, err := modfile.Parse(path.Join(root, "go.mod"), ty.gomod, nil)
		if err != nil {
			ty.why = "go.mod not parsed: " + err.Error()
		} else if f.Module != nil {
			ty.mod = f.Module.Mod.Path
		}
		// go builds -mod=vendor with a vendor/modules.txt (go 1.14 on): its
		// packages are typed from vendor/ as own source, only the standard
		// library from export data; it must agree with go.mod.
		// vendor/ only as the base has it: the gate may build it, or (GOFLAGS
		// -mod=readonly, as the prepared image sets) the module cache.
		vendor := path.Join(root, "vendor") + "/"
		blob, work := t.files[vendor+"modules.txt"], false
		for d := root; ; d = path.Dir(d) { // go finds a go.work upward
			if work = t.files[path.Join(d, "go.work")] != ""; work || d == "." || d == "/" {
				break
			}
		}
		goVersion := "1.16" // go's reading of a go.mod with no go directive
		if err == nil && f.Go != nil {
			goVersion = f.Go.Version
		}
		switch on := err == nil && version.Compare("go"+goVersion, "go1.14") >= 0; {
		case r.vendorChanged[vendor]:
			ty.why = "a file under vendor/ changed"
		case work && (blob != "" || t.vendors[vendor]):
			ty.why = "a go.work beside vendor/: workspace mode does not read it"
		case on && blob != "":
			b, why, err := r.file(blob)
			if ty.vendor, ty.gomod, ty.gosum = vendorPkgs(f, string(b)), []byte("module adc.invalid/vendored\n"), nil; ty.vendor == nil {
				ty.why = cmp.Or(why, errText(err), "vendor/modules.txt does not agree with go.mod")
			}
		case on && t.vendors[vendor]:
			ty.why = "a vendor/ directory without vendor/modules.txt"
		}
	}
	if first, _, _ := strings.Cut(ty.mod, "/"); ty.mod != "" && !strings.Contains(first, ".") && ty.why == "" {
		ty.why = "module path " + ty.mod + " has no dot in its first element: the standard library may shadow it"
	}
	return ty
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// vendorPkgs is the packages vendor/modules.txt lists, if it agrees with
// go.mod as go checks it (each requirement explicit at its version, nothing
// explicit unrequired, no replacement); nil if not.
func vendorPkgs(f *modfile.File, txt string) map[string]bool {
	pkgs, explicit, mod, ver := map[string]bool{}, map[string]string{}, "", ""
	for _, l := range strings.Split(txt, "\n") {
		switch fs := strings.Fields(l); {
		case len(fs) > 3 && fs[0] == "#", len(fs) >= 2 && fs[0] == "##" && fs[1] == "workspace": // a replacement; a workspace's vendoring
			return nil
		case len(fs) == 3 && fs[0] == "#":
			mod, ver = fs[1], fs[2]
		case len(fs) >= 2 && fs[0] == "##" && strings.HasPrefix(fs[1], "explicit"):
			explicit[mod] = ver
		case len(fs) == 1 && mod != "" && (fs[0] == mod || strings.HasPrefix(fs[0], mod+"/")):
			pkgs[fs[0]] = true
		case len(fs) == 1: // a package outside its module (fmt, an own one)
			return nil
		}
	}
	for _, r := range f.Require {
		if explicit[r.Mod.Path] != r.Mod.Version {
			return nil
		}
		delete(explicit, r.Mod.Path)
	}
	if len(explicit) > 0 || len(f.Replace) > 0 {
		return nil
	}
	return pkgs
}

// local is the directory of an own (or vendored) package's import path, if one.
func (ty *typer) local(p string) (string, bool) {
	if first, _, _ := strings.Cut(p, "/"); ty.vendor[p] && strings.Contains(first, ".") { // never the standard library's
		return path.Join(ty.root, "vendor", p), true
	}
	rel, ok := strings.CutPrefix(p, ty.mod)
	if ty.mod == "" || !ok || rel != "" && (rel[0] != '/' || path.Clean(rel) != rel) {
		return "", false
	}
	dir := path.Join(ty.root, strings.TrimPrefix(rel, "/"))
	for d := dir; d != ty.root; d = path.Dir(d) {
		if ty.t.files[path.Join(d, "go.mod")] != "" { // a nested module's
			return "", false
		}
	}
	return dir, true
}

// pathOf is the import path of the own package in dir.
func (ty *typer) pathOf(dir string) string {
	if ty.mod == "" || dir == ty.root {
		return ty.mod
	} else if ty.root == "." {
		return ty.mod + "/" + dir
	}
	return ty.mod + "/" + strings.TrimPrefix(dir, ty.root+"/")
}

// parse reads the files the gate builds of the own package in dir (no
// tests, no cgo), once; why says why not.
func (ty *typer) parse(dir string) ([]*ast.File, string) {
	if fs, ok := ty.parsed[dir]; ok {
		return fs, ty.failed["dir "+dir]
	}
	ty.parsed[dir] = nil
	why := ""
	switch {
	case len(ty.parsed) > maxPkgs:
		why = "over the guard's package bound"
	case ty.r.ctx.Err() != nil:
		why = ty.r.ctx.Err().Error()
	}
	name := ""
	for _, p := range ty.t.dirs[dir] {
		if why != "" || path.Ext(p) != ".go" || strings.HasSuffix(p, "_test.go") {
			continue
		}
		b, unread, err := ty.r.file(ty.t.files[p])
		if why = cmp.Or(unread, errText(err)); why != "" {
			break
		}
		if !builds(ty.platform, p, b) {
			continue
		}
		f, err := parser.ParseFile(ty.fset, p, b, parser.SkipObjectResolution)
		switch {
		case err != nil:
			why = err.Error()
		case f.Name.Name == "documentation" || slices.ContainsFunc(f.Imports, func(is *ast.ImportSpec) bool { return is.Path.Value == `"C"` }):
		case name != "" && name != f.Name.Name:
			why = "files of packages " + name + " and " + f.Name.Name
		default:
			name, ty.parsed[dir] = f.Name.Name, append(ty.parsed[dir], f)
		}
	}
	if why == "" && name == "" {
		why = "no Go files the gate builds"
	}
	if why != "" {
		ty.failed["dir "+dir] = why
	}
	return ty.parsed[dir], why
}

// builds: the gate builds p (src) on pl, by go/build's rules (no tag the
// guard does not model).
func builds(pl [2]string, p string, src []byte) bool {
	c := build.Context{GOOS: pl[0], GOARCH: pl[1], Compiler: "gc", ReleaseTags: release,
		OpenFile: func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(src)), nil }}
	c.ToolTags = append(slices.DeleteFunc(slices.Clone(build.Default.ToolTags), func(t string) bool { return !strings.HasPrefix(t, "goexperiment.") }),
		map[string][]string{"amd64": {"amd64.v1"}, "arm64": {"arm64.v8.0"}}[pl[1]]...) // the toolchain's experiments, pl's default level
	ok, err := c.MatchFile(path.Dir(p), path.Base(p))
	return ok && err == nil
}

// need lists, once, what files import outside the module, through the own
// packages they import.
func (ty *typer) need(files []*ast.File) error {
	seen, ext := map[string]bool{}, []string{}
	var walk func([]*ast.File)
	walk = func(fs []*ast.File) {
		for _, f := range fs {
			for _, is := range f.Imports {
				p, _ := strconv.Unquote(is.Path.Value)
				if dir, own := ty.local(p); !seen[p] && own {
					seen[p] = true
					fs, _ := ty.parse(dir)
					walk(fs)
				} else if first, _, _ := strings.Cut(p, "/"); ty.vendor != nil && strings.Contains(first, ".") {
					ty.listed[p] = Listed{ImportPath: p, Error: &struct{ Err string }{"not in vendor/modules.txt"}} // go builds -mod=vendor
				} else if _, ok := ty.listed[p]; !seen[p] && !ok && p != "C" && p != "unsafe" {
					seen[p], ext = true, append(ext, p)
				}
			}
		}
	}
	walk(files)
	if len(ext) == 0 || ty.why != "" {
		return nil
	}
	slices.Sort(ext)
	got, err := map[string]Listed{}, errors.New("no Go type loader")
	if ty.r.listRuns++; ty.r.lister != nil && ty.r.listRuns > maxListRuns {
		err = errors.New("over the guard's go list bound")
	} else if ty.r.lister != nil {
		ctx, cancel := context.WithDeadline(ty.r.ctx, ty.r.listEnd)
		got, err = ty.r.lister(ctx, ty.platform, ty.gomod, ty.gosum, ext)
		cancel()
	}
	if ctxErr := ty.r.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	for p, l := range got {
		ty.listed[p] = l
	}
	for _, p := range ext {
		if l := ty.listed[p]; l.Export == "" && l.Error == nil {
			ty.listed[p] = Listed{ImportPath: p, Error: &struct{ Err string }{cmp.Or(errText(err), "not listed")}}
		}
	}
	return nil
}

// lookup opens a listed package's export data, within the version's bound.
func (ty *typer) lookup(p string) (io.ReadCloser, error) {
	l := ty.listed[p]
	if l.Export == "" && l.Error != nil {
		return nil, errors.New(l.Error.Err)
	} else if l.Export == "" {
		return nil, errors.New("no export data")
	}
	fi, err := os.Stat(l.Export)
	if err != nil {
		return nil, err
	} else if ty.export -= fi.Size(); ty.export < 0 {
		return nil, errors.New("over the guard's export data bound")
	}
	return os.Open(l.Export)
}

// Import is an import's package: an own one type-checked (bodies skipped;
// any error fails it), or export data.
func (ty *typer) Import(p string) (*types.Package, error) {
	if ty.why != "" {
		return nil, errors.New(ty.why)
	}
	dir, own := ty.local(p)
	if !own {
		return ty.gc.Import(p)
	} else if why := ty.failed[p]; why != "" {
		return nil, errors.New(why)
	} else if pkg, ok := ty.pkgs[p]; ok && pkg == nil {
		return nil, errors.New("an import cycle")
	} else if ok {
		return pkg, nil
	}
	files, why := ty.parse(dir)
	var first error
	if why == "" {
		ty.pkgs[p] = nil
		conf := types.Config{Importer: ty, FakeImportC: true, IgnoreFuncBodies: true, Error: func(err error) { first = cmp.Or(first, err) }}
		ty.pkgs[p] = ty.r.typeCheck(&conf, p, ty.fset, files, nil)
	}
	if first != nil {
		why = "it does not type-check: " + first.Error()
	}
	if why != "" {
		delete(ty.pkgs, p)
		ty.failed[p] = why
		return nil, errors.New(why)
	}
	return ty.pkgs[p], nil
}

// typeCheck runs conf.Check (it cannot be cancelled) by the guard's deadline:
// past it, or on a done context, the guard stops (recovered) and the check is
// abandoned, its CPU the residual (a killable loader is P14e-2b's).
func (r *run) typeCheck(conf *types.Config, path string, fset *token.FileSet, files []*ast.File, info *types.Info) *types.Package {
	if n := abandoned.Load(); n > 0 { // its CPU and memory still taken, and its fault not seen
		by := "unknown"
		if p := abandonedBy.Load(); p != nil {
			by = *p
		}
		panic(fmt.Sprintf("%d type check(s) abandoned by an earlier guard run (%s) still running: not a finding against this change", n, by))
	}
	done, state := make(chan any, 1), new(atomic.Int32) // 0 running, 1 ended, 2 abandoned
	go func() {
		var v any
		defer func() {
			if !state.CompareAndSwap(0, 1) {
				abandoned.Add(-1)
			}
			done <- cmp.Or(recover(), v) // its panic is the guard's
		}()
		v, _ = conf.Check(path, fset, files, info)
	}()
	abandon := func(why any) {
		if state.CompareAndSwap(0, 2) {
			by := fmt.Sprintf("on base %.12s, started %s", r.base, r.started.Format(time.RFC3339))
			abandonedBy.Store(&by)
			abandoned.Add(1)
		}
		panic(why)
	}
	select {
	case v := <-done:
		if pkg, ok := v.(*types.Package); ok {
			return pkg
		}
		panic(v)
	case <-r.ctx.Done():
		abandon(r.ctx.Err())
	case <-time.After(time.Until(r.listEnd)):
		abandon("a type check over the guard's deadline (" + path + ")")
	}
	return nil
}

// abandoned is the type checks given up on that still run: go/types cannot
// be stopped (a killable process is P14e-2c), so while one runs a new Go
// guard run is unverifiable.
var (
	abandoned   atomic.Int64
	abandonedBy atomic.Pointer[string] // the run that last abandoned one
)

type importFunc func(string) (*types.Package, error)

func (f importFunc) Import(p string) (*types.Package, error) { return f(p) }

// typedOn type-checks one package of dir on pl (its files and export data) and
// adds the types' facts to fa.typed, per file, platform and function. Imports
// not loaded, or a type error: unverifiable.
func (r *run) typedOn(pl [2]string, dir, name string, fset *token.FileSet, files []*ast.File, meta map[*ast.File]fileMeta, fa *pkgFacts) error {
	ty := fa.tree.typer(r, dir, pl)
	if err := ty.need(files); err != nil {
		return err
	}
	failed, typeErr, xtest, suffix := "", "", strings.HasSuffix(name, "_test"), ""
	if saved := ty.pkgs; xtest { // as go test builds it, for this check only: what imports the package, against it with its tests
		var reaches func(p *types.Package, seen map[*types.Package]bool) bool
		reaches = func(p *types.Package, seen map[*types.Package]bool) bool {
			if seen[p] = true; p.Path() == ty.pathOf(dir) {
				return true
			}
			return slices.ContainsFunc(p.Imports(), func(q *types.Package) bool { return !seen[q] && reaches(q, seen) })
		}
		suffix, ty.pkgs = "_test", map[string]*types.Package{}
		for k, p := range saved {
			if p != nil && !reaches(p, map[*types.Package]bool{}) {
				ty.pkgs[k] = p
			}
		}
		if pkg := ty.self[ty.pathOf(dir)]; pkg != nil {
			ty.pkgs[ty.pathOf(dir)] = pkg
		}
		defer func() { ty.pkgs = saved }()
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{},
		Instances: map[*ast.Ident]types.Instance{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	// Any error but a name declared again in a file under a tag the guard
	// does not model (race, !race), with the same declared types (redecl).
	redecl, pending := map[string]string{}, ""
	conf := types.Config{FakeImportC: true, Error: func(err error) {
		te, ok := err.(types.Error)
		switch at := fset.Position(te.Pos).String(); {
		case ok && redeclared.MatchString(te.Msg):
			typeErr, pending = cmp.Or(typeErr, map[bool]string{true: "a redeclaration without its other declaration"}[pending != ""]), at
		case ok && otherDecl.MatchString(te.Msg) && pending != "":
			redecl[pending], pending = at, ""
		case ok && methodDecl.MatchString(te.Msg):
			redecl[at] = te.Msg[strings.LastIndex(te.Msg, " at ")+4:]
		case typeErr == "":
			typeErr = err.Error()
		}
	}, Importer: importFunc(func(p string) (*types.Package, error) {
		pkg, err := ty.Import(p)
		if err != nil && failed == "" {
			failed = p + ": " + err.Error()
		}
		return pkg, err
	})}
	pkg := r.typeCheck(&conf, cmp.Or(ty.pathOf(dir), dir)+suffix, fset, files, info)
	if pending != "" {
		typeErr = cmp.Or(typeErr, pending+": a redeclaration without its other declaration")
	}
	shapes := shapes(fset, info, files)
	for again, first := range redecl {
		if typeErr == "" && (shapes[again] == "" || shapes[again] != shapes[first]) {
			typeErr = again + ": declared again with other types (" + first + ")"
		}
	}
	key, where := dir+":"+name, pl[0]+"/"+pl[1]+": " // the first platform's reason is kept
	if err := r.ctx.Err(); err != nil {
		return err
	} else if failed != "" {
		fa.untyped[key] = cmp.Or(fa.untyped[key], where+"the types of its imports not loaded ("+failed[:min(len(failed), 300)]+")")
		return nil
	} else if typeErr != "" { // the gate cannot compile it either: its facts still count
		fa.untyped[key] = cmp.Or(fa.untyped[key], where+"it does not type-check ("+typeErr[:min(len(typeErr), 300)]+")")
	}
	if !xtest && ty.pathOf(dir) != "" {
		ty.self[ty.pathOf(dir)] = pkg
	}
	for _, f := range files {
		for _, d := range f.Decls {
			where := "package level"
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv != nil {
				where = recv(fn) + "." + fn.Name.Name
			} else if ok {
				where = fn.Name.Name
			}
			facts(info, d, func(fact string) { fa.typed[meta[f].path+"\x00"+pl[0]+"/"+pl[1]+" "+where+"\x00"+fact]++ }) // per platform: no trade across them
			ref := func(q string) {                                                                                     // never class 2; cut as facts are
				if len(q) > 200 {
					q = fmt.Sprintf("%s… %x", q[:200], sha256.Sum256([]byte(q)))
				}
				fa.refs[meta[f].path+"\x00"+q+"\x00"+where+"\x00"+pl[0]+"/"+pl[1]]++
			}
			ast.Inspect(d, func(n ast.Node) bool { // by real types, an S type's method through a value (util.Flags().Parse) or an S type argument: warns
				if sel, ok := n.(*ast.SelectorExpr); ok && info.Selections[sel] != nil && info.Selections[sel].Kind() != types.FieldVal { // a method; a field is data
					if o := info.Selections[sel].Obj().(*types.Func); sObj(o) {
						q, recv := o.FullName(), types.TypeString(info.Selections[sel].Recv(), nil) // (*flag.FlagSet).Parse
						if !strings.Contains(q, recv) {
							q += " via " + recv // promoted, or through an alias's or another package's type
						}
						ref(q)
					}
				} else if id, ok := n.(*ast.Ident); ok && info.Instances[id].TypeArgs != nil {
					for t := range info.Instances[id].TypeArgs.Types() {
						if p, ok := types.Unalias(t).(*types.Pointer); ok {
							t = p.Elem()
						}
						if nt, ok := types.Unalias(t).(*types.Named); ok && sObj(nt.Obj()) {
							ref(id.Name + "[" + types.TypeString(t, nil) + "]") // parse[*flag.FlagSet]: its methods are called in the generic
						}
					}
				}
				return true
			})
		}
	}
	return nil
}

// shapes are what a redeclaration must repeat, by its name's position: its
// kind; a signature; a type's alias-ness, type and its file's methods on it; a
// var's or const's type, a const's value but a bool's (race pairs differ in
// one; the 22 and 0x16 rules read values), an implicit (iota) one never.
func shapes(fset *token.FileSet, info *types.Info, files []*ast.File) map[string]string {
	out := map[string]string{}
	of := func(es ...ast.Expr) (s string) {
		for _, e := range es {
			if _, dots := e.(*ast.Ellipsis); e != nil {
				s += map[bool]string{true: "..."}[dots] + types.TypeString(info.TypeOf(e), nil) + ";"
			}
		}
		return s
	}
	fields := func(fls ...*ast.FieldList) (es []ast.Expr) {
		for _, fl := range fls {
			for _, f := range cmp.Or(fl, &ast.FieldList{}).List {
				es = append(es, slices.Repeat([]ast.Expr{f.Type}, max(1, len(f.Names)))...)
			}
		}
		return es
	}
	for _, f := range files {
		methods := map[string][]string{} // a receiver type's methods declared in this file
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv != nil {
				methods[recv(fn)] = append(methods[recv(fn)], fn.Name.Name+" "+of(fields(fn.Recv, fn.Type.TypeParams, fn.Type.Params, fn.Type.Results)...))
			}
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				out[fset.Position(d.Name.Pos()).String()] = "func " + of(fields(d.Recv, d.Type.TypeParams, d.Type.Params, d.Type.Results)...)
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					switch sp := sp.(type) {
					case *ast.TypeSpec:
						out[fset.Position(sp.Name.Pos()).String()] = fmt.Sprintf("type %v %s methods %q", sp.Assign.IsValid(),
							of(append(fields(sp.TypeParams), sp.Type)...), slices.Sorted(slices.Values(methods[sp.Name.Name])))
					case *ast.ValueSpec:
						for i, n := range sp.Names {
							at, v := fset.Position(n.Pos()).String(), ast.Expr(nil)
							if i < len(sp.Values) {
								v = sp.Values[i]
							}
							out[at] = d.Tok.String() + " " + of(sp.Type, v)
							if tv := info.Types[v]; d.Tok == token.CONST && (tv.Value == nil || tv.Value.Kind() != constant.Bool) {
								out[at] += " = " + cmp.Or(tv.Value, constant.MakeUnknown()).ExactString() + map[bool]string{true: " " + at}[tv.Value == nil] // the 22 and 0x16 rules read it
							}
						}
					}
				}
			}
		}
	}
	return out
}

// Facts, each keyed by its text, file and function: a key the candidate
// holds more often than the base, on a platform, blocks (factMakes with no Go
// test anywhere warns), so no counted form can be traded for another.
const (
	factForged  = "a testing value not the runner's"
	factReflect = "reflection on a testing value"
	factMakes   = "reflection that makes, writes or calls values"
	factUnsafe  = "a conversion from unsafe.Pointer" // a reflect.Value's (UnsafePointer) needs no unsafe import
)

var (
	redeclared   = regexp.MustCompile(`^\S+ redeclared in this block$`)
	otherDecl    = regexp.MustCompile(`^\tother declaration of \S+$`)
	methodDecl   = regexp.MustCompile(`^(method|field and method with the same name) \S+ already declared at \S+$`)
	reflectMakes = strings.Fields("New NewAt Zero MakeSlice MakeMap MakeMapWithSize MakeChan MakeFunc Append AppendSlice Copy")
	valueMakes   = strings.Fields("Call CallSlice Method MethodByName Convert Grow Slice Slice3 Addr Pointer UnsafeAddr UnsafePointer Bytes Clear") // from Addr: raw pointers, or a write past the read-only flag
)

// facts reports what the real types show in n: a testing value made (an
// expression, variable, type or type argument holding a T, B, F or M by
// value, a type directly behind * excepted); a value asserted, switched or
// converted to pointers to one, or to an interface with a failure-setting
// method; a type argument a generic could assert a value to
// (as[*testing.T](v)); and reflection reaching a testing type, or making,
// writing or calling values. Types are unaliased: an alias in another,
// unchanged package is seen. Also: a fillable value or type (a decoder's
// &h), and a reflect.Value maker through an interface or a constraint.
func facts(info *types.Info, n ast.Node, add func(string)) {
	starred, called := map[ast.Expr]bool{}, map[*ast.Ident]bool{}
	ast.Inspect(n, func(x ast.Node) bool {
		e, _ := x.(ast.Expr)
		at := func(what string, n ast.Node, of ...types.Type) {
			s := types.ExprString(n.(ast.Expr))
			for _, t := range of {
				s += " of " + types.TypeString(t, nil)
			}
			if len(s) > 200 {
				s = fmt.Sprintf("%s… %x", s[:200], sha256.Sum256([]byte(s)))
			}
			add(what + " (" + s + ")")
		}
		switch x := x.(type) {
		case *ast.StarExpr:
			for e := x.X; ; {
				starred[e] = true
				p, ok := e.(*ast.ParenExpr)
				if !ok {
					break
				}
				e = p.X
			}
		case *ast.TypeAssertExpr:
			if x.Type != nil && launders(info.TypeOf(x.Type)) {
				at(factForged, x, info.TypeOf(x.X))
			}
		case *ast.TypeSwitchStmt:
			var tag types.Type
			ast.Inspect(x.Assign, func(n ast.Node) bool {
				if ta, ok := n.(*ast.TypeAssertExpr); ok && tag == nil {
					tag = info.TypeOf(ta.X)
				}
				return tag == nil
			})
			for _, c := range x.Body.List {
				for _, e := range c.(*ast.CaseClause).List {
					if info.Types[e].IsType() && launders(info.Types[e].Type) {
						at(factForged, e, tag)
					}
				}
			}
		case *ast.Ident: // a reflect function as a value (vo := reflect.ValueOf) hides its calls: one like the makers
			if o, _ := info.Uses[x].(*types.Func); o != nil && o.Pkg() != nil && o.Pkg().Path() == "reflect" && (!called[x] ||
				o.FullName() == "reflect."+o.Name() && slices.Contains(reflectMakes, o.Name()) || strings.HasPrefix(o.FullName(), "(reflect.Value).") &&
				(strings.HasPrefix(o.Name(), "Set") || slices.Contains(valueMakes, o.Name()))) {
				at(factMakes, x)
			} else if o != nil && valueMethod(o) {
				at(factMakes, x) // reflect.Value's, through an interface or a constraint
			}
			if o := info.Defs[x]; o != nil && (holds(o.Type(), false, nil) || fillable(o.Type())) && (isVar(o) || isTypeName(o)) {
				add(factForged + " (" + x.Name + " " + types.TypeString(o.Type(), nil) + ")")
			}
			if in, ok := info.Instances[x]; ok && slices.ContainsFunc(slices.Collect(in.TypeArgs.Types()), func(t types.Type) bool {
				return holds(t, false, nil) || fillable(t) || testingPtr(t) || failing(t)
			}) {
				add(factForged + " (" + x.Name + types.TypeString(in.Type, nil) + ")")
			} else if ok && slices.ContainsFunc(slices.Collect(in.TypeArgs.Types()), isUnsafe) {
				add(factUnsafe + " (" + x.Name + types.TypeString(in.Type, nil) + ")") // a generic may convert it (deref[unsafe.Pointer, T])
			}
		case *ast.CallExpr:
			if tv := info.Types[ast.Unparen(x.Fun)]; tv.IsType() && len(x.Args) == 1 && (!types.IsInterface(tv.Type) || rawParam(tv.Type)) { // to a holder of *testing.T, of anything else
				if from := info.TypeOf(x.Args[0]); holds(tv.Type, true, nil) && (from == nil || !types.Identical(from, tv.Type)) {
					at(factForged, x, from)
				} else if from != nil && (isUnsafe(from) && !isUnsafe(tv.Type) || ptrParam(from) && pointerLike(tv.Type) || ptrParam(tv.Type) && pointerLike(from)) {
					at(factUnsafe, x)
				}
			}
			fun, _ := fn(x.Fun)
			id := ident(fun)
			if sel, ok := fun.(*ast.SelectorExpr); ok {
				id = sel.Sel
			}
			if o, _ := info.Uses[id].(*types.Func); o != nil && strings.HasPrefix(o.FullName(), "encoding/gob.Register") &&
				reaches(info, x, id, func(t types.Type) bool { return holds(t, true, nil) }) {
				at(factForged, x) // a stream may then allocate one
			}
			if o, _ := info.Uses[id].(*types.Func); o != nil && o.Pkg() != nil && o.Pkg().Path() == "reflect" {
				if reaches(info, x, id, func(t types.Type) bool { return holds(t, true, nil) }) {
					at(factReflect, x)
				}
				called[id] = true
			}
		}
		if tv, ok := info.Types[e]; ok && (tv.IsValue() || tv.IsType() && !starred[e]) && (holds(tv.Type, false, nil) || fillable(tv.Type)) {
			at(factForged, e)
		}
		return true
	})
}

func isVar(o types.Object) bool { _, ok := o.(*types.Var); return ok }

// sObj: o is an S package's (testing's, reflect's and unsafe's have their own rules).
func sObj(o types.Object) bool {
	return o.Pkg() != nil && sPkg(o.Pkg().Path()) && !slices.Contains([]string{"testing", "reflect", "unsafe"}, o.Pkg().Path())
}
func isTypeName(o types.Object) bool { _, ok := o.(*types.TypeName); return ok }

// reaches: a call's arguments (through a conversion to an interface,
// any(h)) or type arguments (TypeFor[holder]) are of a type that is; for
// reflect, one holding a testing type at any depth, behind pointers too.
func reaches(info *types.Info, c *ast.CallExpr, id *ast.Ident, is func(types.Type) bool) bool {
	var ts []types.Type
	for _, a := range c.Args {
		ts = append(ts, info.TypeOf(a))
		if conv, ok := ast.Unparen(a).(*ast.CallExpr); ok && len(conv.Args) == 1 && info.Types[ast.Unparen(conv.Fun)].IsType() {
			ts = append(ts, info.TypeOf(conv.Args[0]))
		}
	}
	if in, ok := info.Instances[id]; ok {
		ts = append(ts, slices.Collect(in.TypeArgs.Types())...)
	}
	return slices.ContainsFunc(ts, func(t types.Type) bool { return t != nil && is(t) })
}

// fillable: a decoder given a value of t (json.Unmarshal(b, &h)) can store a
// testing value it made: a path to a T, B, F or M through pointers, arrays,
// slices, maps, chans and exported or embedded fields; not *testing.T itself.
func fillable(t types.Type) bool {
	var slot func(t types.Type, seen map[types.Type]bool) bool
	slot = func(t types.Type, seen map[types.Type]bool) bool {
		if t = types.Unalias(t); t == nil || seen[t] {
			return false
		} else if seen[t] = true; testingStruct(t) {
			return true
		}
		switch u := t.Underlying().(type) {
		case *types.Struct:
			for i := range u.NumFields() {
				if (u.Field(i).Exported() || u.Field(i).Embedded()) && slot(u.Field(i).Type(), seen) { // an embedded one promotes
					return true
				}
			}
		case *types.Map:
			return slot(u.Key(), seen) || slot(u.Elem(), seen)
		case *types.Pointer, *types.Slice, *types.Array, *types.Chan:
			return slot(u.(interface{ Elem() types.Type }).Elem(), seen)
		}
		return false
	}
	return t != nil && !testingPtr(t) && slot(t, map[types.Type]bool{})
}

// testingPtr: t is *testing.T, B, F or M (a value of it may be the runner's).
func testingPtr(t types.Type) bool {
	p, ok := types.Unalias(t).(*types.Pointer)
	return ok && testingStruct(types.Unalias(p.Elem()))
}

func testingStruct(t types.Type) bool {
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "testing" && slices.Contains([]string{"T", "B", "F", "M", "common"}, n.Obj().Name())
}

// valueMethod: o is an interface's (or constraint's) method a reflect.Value
// maker satisfies: its name, over reflect's types (s.Set(v)), or a raw
// pointer's maker by its exact signature (interface{ UnsafeAddr() uintptr }).
func valueMethod(o types.Object) bool {
	f, ok := o.(*types.Func)
	if !ok || f.Signature().Recv() == nil || !types.IsInterface(f.Signature().Recv().Type()) {
		return false
	} else if want, ok := rawMakers[f.Name()]; ok { // by type: an alias or a type-parameter result too
		sig := f.Signature()
		if sig.Params().Len() > 0 || sig.Results().Len() != b2i(want != nil) {
			return false
		} else if want == nil {
			return true
		}
		r := types.Unalias(sig.Results().At(0).Type())
		_, param := r.(*types.TypeParam)
		return param || want(r)
	}
	return (strings.HasPrefix(f.Name(), "Set") || slices.Contains(valueMakes, f.Name())) && strings.Contains(types.TypeString(f.Type(), nil), "reflect.")
}

// rawMakers is reflect.Value's raw-pointer makers, by their one result (none: Clear; a named result too).
var rawMakers = map[string]func(types.Type) bool{
	"Pointer": isUintptr, "UnsafeAddr": isUintptr, "UnsafePointer": isUnsafe,
	"Bytes": func(t types.Type) bool { return types.Identical(t.Underlying(), types.NewSlice(types.Typ[types.Byte])) }, "Clear": nil,
	"Addr": func(t types.Type) bool {
		n, ok := t.(*types.Named)
		return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "reflect" && n.Obj().Name() == "Value"
	},
}

// isUintptr, like isUnsafe and Bytes, by the underlying type: a named one
// (type U uintptr) is still a raw address (a false block: no reflect.Value
// method has it).
func isUintptr(t types.Type) bool { return types.Identical(t.Underlying(), types.Typ[types.Uintptr]) }

func isUnsafe(t types.Type) bool {
	return types.Identical(t.Underlying(), types.Typ[types.UnsafePointer])
}

// rawParam: t is a type parameter whose constraint has a type set (P ~unsafe.Pointer), not a pure method set.
func rawParam(t types.Type) bool {
	p, ok := types.Unalias(t).(*types.TypeParam)
	if !ok {
		return false
	}
	i, ok := p.Constraint().Underlying().(*types.Interface)
	return ok && !i.IsMethodSet()
}

func pointerLike(t types.Type) bool {
	_, ptr := t.Underlying().(*types.Pointer)
	return ptr || ptrParam(t) || isUnsafe(t) || types.Identical(t.Underlying(), types.Typ[types.Uintptr])
}

// ptrParam: t is a type parameter whose type set has an unsafe.Pointer or a
// pointer term (P ~unsafe.Pointer | ~uintptr), through embedded constraints;
// a numeric one (N ~int | ~uintptr) is no raw address. Past a depth: one.
func ptrParam(t types.Type) bool {
	p, ok := types.Unalias(t).(*types.TypeParam)
	var raw func(types.Type, int) bool
	raw = func(t types.Type, depth int) bool {
		switch u := t.Underlying().(type) {
		case *types.Interface:
			return depth > 8 || slices.ContainsFunc(slices.Collect(u.EmbeddedTypes()), func(e types.Type) bool { return raw(e, depth+1) })
		case *types.Union:
			for i := range u.Len() {
				if raw(u.Term(i).Type(), depth+1) {
					return true
				}
			}
			return false
		case *types.Pointer:
			return true
		}
		return isUnsafe(t)
	}
	return ok && raw(p.Constraint(), 0)
}

// launders: a value asserted or switched to t is a testing value to the
// guard: t holds one at any depth (**testing.T, []*testing.T, a field), or
// is an interface with a failure-setting method (failing).
func launders(t types.Type) bool {
	return t != nil && (holds(t, true, nil) || failing(t))
}

// failing: t is an interface with a failure-setting method (testing.TB,
// interface{ Error(...any) }, a type parameter so constrained).
func failing(t types.Type) bool {
	if p, ok := types.Unalias(t).(*types.TypeParam); ok {
		t = p.Constraint()
	}
	if t == nil {
		return false
	} else if u, ok := types.Unalias(t).Underlying().(*types.Interface); ok {
		for i := range u.NumMethods() {
			if s := failSigs[u.Method(i).Name()]; s != nil && types.Identical(u.Method(i).Type(), s) {
				return true
			}
		}
	}
	return false
}

// holds: t holds a testing.T, B, F or M (or their common part) by value,
// through arrays, slices, maps, chans, fields and signatures; ptr, also
// behind pointers, and testing.TB.
func holds(t types.Type, ptr bool, seen map[types.Type]bool) bool {
	if t = types.Unalias(t); t == nil || seen[t] {
		return false
	} else if seen == nil {
		seen = map[types.Type]bool{}
	}
	seen[t] = true
	if n, ok := t.(*types.Named); testingStruct(t) || ok && ptr && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "testing" && n.Obj().Name() == "TB" {
		return true
	}
	var in []types.Type
	switch u := t.Underlying().(type) {
	case *types.Struct:
		for i := range u.NumFields() {
			in = append(in, u.Field(i).Type())
		}
	case *types.Map:
		in = []types.Type{u.Key(), u.Elem()}
	case *types.Signature:
		in = []types.Type{u.Params(), u.Results()}
	case *types.Tuple:
		for i := range u.Len() {
			in = append(in, u.At(i).Type())
		}
	case *types.Pointer:
		if ptr {
			in = []types.Type{u.Elem()}
		}
	case interface{ Elem() types.Type }: // array, slice, chan
		in = []types.Type{u.Elem()}
	}
	return slices.ContainsFunc(in, func(t types.Type) bool { return t != nil && holds(t, ptr, seen) })
}
