// SPDX-License-Identifier: Apache-2.0

// The guard's loader: go list in the prepared image, its output checked,
// for the type checks' imports.
//
// P14e-attr (#226 entries 148, 168, 170 and 176): the guard's own failures
// are the host's, its run's error (ErrGuardRun), never a refusal, so they
// never repair the build: the child not dispatched, not started or ended
// without its result; a panic in the guard or its service; the type
// check's or the whole run's deadline; and every listing failure but one,
// candidateListing's (Decision A, an allowlist): of the candidate's
// version; the base listed cleanly on the same platform in the same guard
// run; exactly one listFailure, nothing joined to it; and that failure
// clean: an input refused (listable, its module paths valid), a bound
// alone (cleanBound), or go list's own recorded status 1-124 ($r/golist)
// with its list read completely within its bound and no kill line in it
// (killedStep). Its acting half is closed. What remains:
//   - a host fault read as the candidate's, each a refusal that repairs the
//     build, up to the rounds rail: a host fault that strikes only the
//     candidate's listing, after the base's succeeded on the platform, and
//     exits go list 1-124 with a whole list naming no kill (a full results
//     mount mid-write; a module cache entry only the candidate's go.mod
//     needs, missing from the image; a step inside go list killed by a
//     signal and reported as its exit 1, its list naming no signal); and an
//     export-data read failing inside the type checks;
//   - the reverse, a candidate's defect read as the host's (Opus #412 r2
//     L1), each leaving the accept held after its retries ("runner:
//     failed"), never repaired, never accepted: a listing past its bounds;
//     a bound broken with anything joined to it; the candidate's listing
//     spending the run's shared copy budget (maxRunCopy) so that a later
//     base listing is refused; a candidate's failure on a platform the
//     base never listed; a kill line a candidate's diagnostics could still
//     print exactly.
package accept

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/fsread"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// GoEnv is go's whole Go environment in the prepared image (P14e-2c): the
// loader's go list here, and the gate's go test (P15), so the guard types
// what the gate builds. One toolchain: a candidate's raised toolchain line
// never switches it. The module cache only: go.mod read only, no proxy, no
// checksum database, no workspace (a base go.work is never read, vendor/ or
// not). No cgo; the default experiments and each architecture's default
// level, as the guard's build constraints model them.
var GoEnv = []string{"GOFLAGS=-mod=readonly -buildvcs=false", "GOTOOLCHAIN=local", "GOWORK=off", "GOPROXY=off", "GOSUMDB=off",
	"GOENV=off", "GOTELEMETRY=off", "GODEBUG=goindex=0", "CGO_ENABLED=0", "GO111MODULE=on", "GOEXPERIMENT=", "GOAMD64=v1", "GOARM64=v8.0"}

// goImage is the prepared image's layout (fixtures/m1/images/go/Containerfile);
// no home: go reads no configuration (GOENV=off) and writes no cache there.
var goImage = []string{"PATH=/usr/local/go/bin:/usr/bin:/bin", "GOPATH=/go", "GOCACHE=/opt/adc/gocache"}

// imageGOROOT is the prepared image's GOROOT (goImage's PATH): its tools'
// diagnostics name it (killedStep).
const imageGOROOT = "/usr/local/go"

// listScript lists "$@" into $1/list, a package a line (its path, export
// data file and quoted error), copies into $1/x the export data whose file
// name $1/have does not hold (the cache names it by its content), and
// writes go's version into $1/version. A miss in the image's read-only
// build cache (a platform or package it was not warmed for: an error
// naming the cache or a read-only file system) lists again with a cold
// cache in the run's /tmp. go list's own status is $1/golist (Opus #412
// B3): nonzero, the script stops there and exits 0; every other failure of
// the script's own exits listScriptFailed, never go list's.
const listScript = `set -u
r=$1
shift
go env GOVERSION >"$r/version" || exit 3
f='{{.ImportPath}}{{"\t"}}{{.Export}}{{"\t"}}{{with .Error}}{{printf "%q" .Err}}{{end}}'
: >"$r/list" || exit 3
go list -e -export -deps -f "$f" -- "$@" >"$r/list"
s=$?
if [ "$s" -eq 0 ] && cut -f3 "$r/list" | grep -qF -e 'read-only file system' -e "$GOCACHE"; then
  GOCACHE=/tmp/gocache go list -e -export -deps -f "$f" -- "$@" >"$r/list"
  s=$?
fi
echo "$s" >"$r/golist" || exit 3
[ "$s" -eq 0 ] || exit 0
mkdir "$r/x" || exit 3
awk -F '\t' 'NR == FNR { h[$0] = 1; next } $2 != "" { n = $2; sub(/.*\//, "", n); if (!(n in h)) print $2 }' "$r/have" "$r/list" |
  xargs -r -d '\n' cp -t "$r/x/" -- || exit 3
rm -- "$r/have" || exit 3
`

// SandboxLister is the production Lister (operator answer "loader b"): go
// list -export runs in img, the prepared Go image, through h's sandbox in
// gate mode (no network; only the run's workspace and results mounted),
// under GoEnv, with only go.mod and go.sum in its workspace. Only the list
// and the export data come out, into the guard run's directory (Integrity's
// scratch: h.Root's runs/), each export data file once a run, all within
// maxRunCopy. go's release must be the guard's (one export data format);
// lim bounds each run.
func SandboxLister(h sandbox.Host, img sandbox.Image, lim sandbox.Limits) Lister {
	return boxLister(h, img, lim)
}

// maxRunCopy bounds what one guard run's listings copy out, in all: the
// export data both versions' typers may read, and each listing's list.
func maxRunCopy() int64 { return 2*maxExport + maxList*int64(maxListRuns) }

// box is the sandbox a loader runs in (a sandbox.Host).
type box interface {
	NewAttempt() (*sandbox.Attempt, error)
	Start(ctx context.Context, r sandbox.Run) ([]byte, error)
}

func boxLister(h box, img sandbox.Image, lim sandbox.Limits) Lister {
	return func(ctx context.Context, dir string, pl [2]string, gomod, gosum []byte, paths []string) (map[string]Listed, error) {
		got, err := boxList(ctx, h, img, lim, dir, pl, gomod, gosum, paths)
		if _, ok := err.(*listFailure); err != nil && !ok { // a listFailure is passed on as it is: candidateListing reads it whole
			return nil, fmt.Errorf("go list in the prepared image: %w", err)
		}
		return got, err
	}
}

// maxListEntries bounds a listing's packages; exportName is a build
// cache's export data file, named by its content's sha256.
var (
	maxListEntries       = 1 << 14
	maxVersion     int64 = 256 // go env GOVERSION's line
	exportName           = regexp.MustCompile(`^[0-9a-f]{64}-d$`)
)

// boxList lists in the sandbox. What comes out is checked for its form,
// its bounds and its consistency, not its provenance: the pinned image and
// go.sum are trusted. Every listing's copy is charged to the run's ledger,
// kept or not; past maxRunCopy, a listing fails.
//
// What its failure is: a listFailure, what it observed of a failure that
// may be the candidate's (its input refused, listable; a bound its run or
// its copy broke; go list's own recorded status, nonzero), for
// candidateListing alone to decide (Decision A, #226 entry 176), or
// ErrGuardRun, at its source or by default: everything else, and anything
// joined to a listFailure (a teardown).
func boxList(ctx context.Context, h box, img sandbox.Image, lim sandbox.Limits, dir string, pl [2]string, gomod, gosum []byte, paths []string) (_ map[string]Listed, err error) {
	defer func() {
		if _, ok := err.(*listFailure); err != nil && !ok && !errors.Is(err, ErrGuardRun) {
			err = fmt.Errorf("%w: %w", ErrGuardRun, err)
		}
	}()
	if err := listable(gomod, paths); err != nil {
		return nil, &listFailure{kind: refusalInput, err: err}
	}
	x, ledger := filepath.Join(dir, "x"), filepath.Join(dir, "copied") // the run's export data, by name; what its listings copied out
	if err := os.MkdirAll(x, 0o700); err != nil {
		return nil, err
	}
	var charged int64
	if b, err := os.ReadFile(ledger); err == nil {
		charged, err = strconv.ParseInt(string(b), 10, 64)
		if err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	have, err := os.ReadDir(x)
	if err != nil {
		return nil, err
	} else if charged >= maxRunCopy() {
		return nil, &listFailure{kind: refusalBound, err: fmt.Errorf("%w: over the guard run's export data bound", sandbox.ErrBound)}
	}
	held := map[string]bool{}
	for _, e := range have {
		held[e.Name()] = true
	}
	a, err := h.NewAttempt()
	if err != nil {
		return nil, err
	}
	defer func() {
		if terr := a.Teardown(); terr != nil { // the controller's, whatever the run's end was
			err = errors.Join(err, fmt.Errorf("%w: the run's teardown: %w", ErrGuardRun, terr))
		}
	}()
	if gomod == nil {
		gomod = []byte("module adc.invalid/guard\n")
	}
	err = os.WriteFile(filepath.Join(a.Workspace(), "go.mod"), gomod, 0o600)
	if err == nil && gosum != nil {
		err = os.WriteFile(filepath.Join(a.Workspace(), "go.sum"), gosum, 0o600)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(a.Results(), "have"), []byte(strings.Join(slices.Sorted(maps.Keys(held)), "\n")+"\n"), 0o600)
	}
	if err != nil {
		return nil, err
	}
	argv := slices.Concat([]string{"env", "-i"}, goImage, GoEnv, []string{"GOOS=" + pl[0], "GOARCH=" + pl[1], "sh", "-c", listScript, "sh", sandbox.ResultsDir}, paths)
	if _, err := h.Start(ctx, sandbox.Run{Image: img, Mode: sandbox.Gate, Attempt: a, Limits: lim, Argv: argv}); errors.Is(err, sandbox.ErrLeft) {
		return nil, &listFailure{kind: refusalBound, err: err} // whatever is joined to it: candidateListing's (cleanBound)
	} else if err != nil {
		return nil, fmt.Errorf("%w: go list did not run to its own end in the sandbox: %w", ErrGuardRun, err)
	}
	out, err := os.MkdirTemp(dir, "list-")
	if err != nil {
		return nil, err
	}
	out = filepath.Join(out, "r")
	cerr := a.CopyOut(out, maxRunCopy()-charged)
	_ = filepath.WalkDir(out, func(_ string, d fs.DirEntry, _ error) error { // charged, whatever comes of it
		if fi, err := d.Info(); err == nil && !d.IsDir() {
			charged += fi.Size()
		}
		return nil
	})
	if err := os.WriteFile(ledger, []byte(strconv.FormatInt(charged, 10)), 0o600); err != nil {
		return nil, err
	}
	if errors.Is(cerr, sandbox.ErrBound) {
		return nil, &listFailure{kind: refusalBound, err: cerr} // whatever is joined to it: candidateListing's (cleanBound)
	} else if cerr != nil {
		return nil, cerr
	}
	status, err := goListStatus(filepath.Join(out, "golist")) // go list's own status, as copied out (Opus #412 B3)
	if err != nil {
		return nil, err
	}
	list, lerr := fsread.Regular(filepath.Join(out, "list"), maxList)
	killed := listKilled(list, imageGOROOT, img.OS+"_"+img.Arch)
	if status != 0 {
		return nil, &listFailure{kind: refusalStatus, err: fmt.Errorf("go list exited %d", status), status: status, complete: lerr == nil, killed: killed}
	} else if lerr != nil {
		return nil, fmt.Errorf("a list over its bound or unread: %w", lerr) // never the candidate's (Astra #418 H3)
	} else if killed {
		return nil, errors.New("a step inside go list killed by a signal") // whatever go list's status (Opus #412 r2 L2)
	}
	if err := checkOut(out, x, held); err != nil {
		return nil, err
	}
	return readListing(out, x, list)
}

// listFailure is what boxList observed of a listing's failure that may be
// the candidate's, whole. Only candidateListing decides whether it is.
type listFailure struct {
	kind     refusalKind
	err      error // the failure as observed, every error joined to it
	status   int   // go list's recorded status (refusalStatus)
	complete bool  // the list read whole within maxList (refusalStatus)
	killed   bool  // a kill line anywhere in the list (refusalStatus)
}

type refusalKind int

const (
	refusalInput  refusalKind = iota + 1 // the input, refused before go runs (listable)
	refusalBound                         // a bound its run (sandbox.ErrLeft) or its copy (sandbox.ErrBound) broke
	refusalStatus                        // go list's own recorded status, nonzero
)

func (f *listFailure) Error() string {
	return "go list in the prepared image: the listing failed on the candidate: " + f.err.Error()
}

func (f *listFailure) Unwrap() error { return f.err }

// candidateListing is Decision A's one allowlist predicate (the lead's
// decision, #226 entry 176; Astra #418): l's failure is the candidate's
// only when every condition holds, and anything else is the host's
// (ErrGuardRun):
//   - it is of the candidate's version;
//   - the base listed cleanly on the same platform in the same guard run
//     (baseOK, #226 entry 170);
//   - its error is exactly one listFailure, nothing joined to it;
//   - that failure is clean: an input refused; a bound alone (cleanBound);
//     or go list's own recorded status 1-124, the list read completely
//     within its bound, no kill line anywhere in it.
func candidateListing(l listing, baseOK bool) bool {
	f, ok := l.err.(*listFailure)
	if !ok || l.base || !baseOK {
		return false
	}
	switch f.kind {
	case refusalInput:
		return true
	case refusalBound:
		return cleanBound(f.err)
	case refusalStatus:
		return f.status >= 1 && f.status <= 124 && f.complete && !f.killed
	}
	return false
}

// cleanBound reports whether err is a bound alone (sandbox.ErrLeft or
// sandbox.ErrBound): every cause in it, through every wrap and join, the
// sandbox's own bound and spec marks, or, a run's (ErrLeft), a permission
// the candidate's workspace denied (sandbox's candidates). A signal, a
// close or other I/O error, an unmeasured tree, a cleanup or the guard's
// own failure joined to it is not (Astra #418 H1).
func cleanBound(err error) bool {
	left := errors.Is(err, sandbox.ErrLeft)
	if !left && !errors.Is(err, sandbox.ErrBound) {
		return false
	}
	var clean func(error) bool
	clean = func(e error) bool {
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			return !slices.ContainsFunc(u.Unwrap(), func(e error) bool { return !clean(e) })
		case interface{ Unwrap() error }:
			if w := u.Unwrap(); w != nil {
				return clean(w)
			}
		}
		return e == sandbox.ErrLeft || e == sandbox.ErrBound || e == sandbox.ErrSpec || left && errors.Is(e, fs.ErrPermission)
	}
	return clean(err)
}

// goListStatus is go list's own status, the file at p the script wrote
// (fsread.Regular, within statusBound); the host's (ErrGuardRun) for a
// missing, oversized or incomplete file: only a whole status line counts
// (Opus #412 B2, B3; Astra chunk 5b M4).
func goListStatus(p string) (int, error) {
	b, err := fsread.Regular(p, statusBound)
	if err != nil {
		return 0, fmt.Errorf("%w: go list's status: %w", ErrGuardRun, err)
	}
	m := statusLine.FindSubmatch(b)
	if m == nil {
		return 0, fmt.Errorf("%w: go list's status %.16q is no status line", ErrGuardRun, b)
	}
	code, _ := strconv.Atoi(string(m[1]))
	return code, nil
}

// statusBound and statusLine: a status file is one decimal status of at
// most three digits and its newline, nothing else.
const statusBound = 4

var statusLine = regexp.MustCompile(`\A(0|[1-9][0-9]{0,2})\n\z`)

// killedStep reports whether why, one listed package's error (unquoted),
// is the pinned toolchain's own termination diagnostic for one of its
// tools killed by a signal (the compiler OOM-killed under the container's
// memory limit): a line that is exactly "<goroot>/pkg/tool/<host>/<tool>:
// signal: killed", host the toolchain's GOOS_GOARCH. The host's, never the
// candidate's (Opus #412 r2 L2); never a substring a candidate's
// diagnostics could carry (Astra chunk 5b M3: a module path or replacement
// naming it).
func killedStep(why, goroot, host string) bool {
	dir := path.Join(goroot, "pkg", "tool", host) + "/"
	for line := range strings.Lines(why) {
		tool, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), dir)
		if name, killed := strings.CutSuffix(tool, ": signal: killed"); ok && killed && slices.Contains(toolNames, name) {
			return true
		}
	}
	return false
}

// toolNames are the toolchain's tools go list -export runs.
var toolNames = []string{"compile", "asm", "cgo", "link", "pack"}

// listKilled reports whether list, a copied-out list, holds a package
// whose error column is killedStep's diagnostic. goroot and host are the
// image's.
func listKilled(list []byte, goroot, host string) bool {
	for line := range strings.Lines(string(list)) {
		cols := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		if len(cols) != 3 || cols[2] == "" {
			continue
		}
		if why, err := strconv.Unquote(cols[2]); err == nil && killedStep(why, goroot, host) {
			return true
		}
	}
	return false
}

// readListing reads a listing's version, bounded as it is read, and its
// list, already read within maxList (boxList), the list's records counted
// and cut, never split, as they are parsed (Astra #287 r2 #3, #7). A
// listing past a bound is the host's: an oversized listing is never the
// candidate's refusal (#226 entry 176).
func readListing(out, x string, list []byte) (map[string]Listed, error) {
	v, err := fsread.Regular(filepath.Join(out, "version"), maxVersion)
	if err != nil {
		return nil, fmt.Errorf("a version over its bound or unread: %w", err)
	} else if v := strings.TrimSpace(string(v)); v != runtime.Version() {
		return nil, fmt.Errorf("the toolchain is %.40q, not the guard's %s", v, runtime.Version())
	}
	got, n := map[string]Listed{}, 0
	for line := range strings.Lines(string(list)) {
		ip, rest, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "\t")
		exp, col, ok2 := strings.Cut(rest, "\t")
		if n++; n > maxListEntries {
			return nil, errors.New("a list over its bound")
		} else if !ok || !ok2 || strings.Contains(col, "\t") {
			return nil, errors.New("an unreadable list")
		}
		l, why := Listed{ImportPath: ip}, ""
		if col != "" { // an error column, present: an error, even an empty one (Astra #287 r2 #4)
			if why, err = strconv.Unquote(col); err != nil {
				return nil, errors.New("an unreadable list")
			}
			why = cmp.Or(why, "an empty error listed")
		}
		switch e := path.Base(exp); {
		case why != "": // an error, whatever else the entry holds (Astra #287 H2)
		case got[ip].ImportPath != "":
			why = "listed twice"
		case exp != "" && (!exportName.MatchString(e) || !regular(filepath.Join(x, e))):
			why = "its export data not copied out"
		case exp != "":
			l.Export = filepath.Join(x, e) // the run's copy of that name, never the path go listed
		}
		if why != "" {
			l.Error = &struct{ Err string }{why}
		}
		got[ip] = l
	}
	return got, nil
}

// checkOut moves the archives a listing copied out into the run's x: only
// list, version and x/ came out, each archive named by its content's
// sha256, none the run held.
func checkOut(out, x string, held map[string]bool) error {
	entries, err := os.ReadDir(out)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !slices.Contains([]string{"list", "version", "x", "golist"}, e.Name()) {
			return fmt.Errorf("an unexpected output (%.40q)", e.Name())
		}
	}
	news, err := os.ReadDir(filepath.Join(out, "x"))
	for _, e := range news {
		if err != nil {
			break
		}
		h, f := sha256.New(), (*os.File)(nil)
		if f, err = os.Open(filepath.Join(out, "x", e.Name())); err == nil {
			_, err = io.Copy(h, f)
			err = errors.Join(err, f.Close())
		}
		switch {
		case err != nil:
		case !e.Type().IsRegular() || !exportName.MatchString(e.Name()) || hex.EncodeToString(h.Sum(nil))+"-d" != e.Name():
			err = fmt.Errorf("an archive not named by its content (%.40q)", e.Name())
		case held[e.Name()]:
			err = fmt.Errorf("an archive the run holds (%.40q)", e.Name())
		default:
			err = os.Rename(filepath.Join(out, "x", e.Name()), filepath.Join(x, e.Name()))
		}
	}
	return err
}

// regular: p is a regular file, not followed if a link.
func regular(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode().IsRegular()
}
