// SPDX-License-Identifier: Apache-2.0

// The guard's loader: go list in the prepared image, its output checked,
// for the type checks' imports.
//
// P14e-attr (#226 entries 148, 168 and 170): the guard's own failures are
// the host's, its run's error (ErrGuardRun), never a refusal, so they never
// repair the build: the child not dispatched, not started or ended without
// its result; a panic in the guard or its service; the type check's or the
// whole run's deadline; and every listing failure but one. A listing's
// failure is the candidate's only when it is of the candidate's version,
// on evidence against its code (errRefused: go list's own exit 1-124 as
// the list script records it, $r/golist; a measured bound its run broke;
// an input refused, listable), and the base's listing on the same
// platform succeeded in the same guard run (listVerdict). A signal, the
// script's own failures, an unmeasured tree, a cleanup, a step inside
// go list killed by a signal as its list records it ("signal: killed",
// killedStep: Opus #412 r2 L2), the base's own failure, and a candidate's
// listing on a platform the base never listed are the host's. Its acting
// half is closed. What remains:
//   - a host fault read as the candidate's, each a refusal that repairs the
//     build, up to the rounds rail: a host fault that strikes only the
//     candidate's listing, after the base's succeeded on the platform, and
//     exits go list 1-124 (a full results mount mid-write; a module cache
//     entry only the candidate's go.mod needs, missing from the image; a
//     step inside go list killed by a signal and reported as its exit 1,
//     its list naming no signal); and an export-data read failing inside
//     the type checks;
//   - the reverse, a candidate's defect read as the host's (Opus #412 r2
//     L1), each leaving the accept held after its retries ("runner:
//     failed"), never repaired, never accepted: the candidate's listing
//     spending the run's shared copy budget (maxRunCopy) so that a later
//     base listing is refused; a candidate's failure on a platform the
//     base never listed.
package accept

import (
	"bytes"
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
	"syscall"

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
		if err != nil {
			return nil, fmt.Errorf("go list in the prepared image: %w", err)
		}
		return got, nil
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
// What its failure is (Lead decision, #226 entry 147): the candidate's
// (errRefused, a block) only on positive evidence: its input refused
// (listable), go list's own exit status observed and nonzero
// (sandbox.WorkloadExit), or a bound its run broke (sandbox.ErrLeft,
// sandbox.ErrBound, the run's copy bound, the list's bounds). Everything
// else is ErrGuardRun, at its source or by default: the attempt's
// directories and files, the copy's and the ledger's I/O, a teardown, a
// client killed or failing itself, no status, a cleanup error joined, and
// any error not classified.
func boxList(ctx context.Context, h box, img sandbox.Image, lim sandbox.Limits, dir string, pl [2]string, gomod, gosum []byte, paths []string) (_ map[string]Listed, err error) {
	defer func() {
		if err != nil && !errors.Is(err, ErrGuardRun) && !errors.Is(err, errRefused) {
			err = fmt.Errorf("%w: %w", ErrGuardRun, err)
		}
	}()
	if err := listable(gomod, paths); err != nil {
		return nil, refused(err)
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
		return nil, refused(errors.New("over the guard run's export data bound"))
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
	if _, err := h.Start(ctx, sandbox.Run{Image: img, Mode: sandbox.Gate, Attempt: a, Limits: lim, Argv: argv}); err != nil {
		if workloadFailed(err) {
			return nil, refused(err)
		}
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
		return nil, refused(cerr)
	} else if cerr != nil {
		return nil, cerr
	}
	if err := listKilled(filepath.Join(out, "list")); err != nil { // a step killed by a signal: the host's, whatever go list's status (Opus #412 r2 L2)
		return nil, err
	}
	if err := goListStatus(filepath.Join(out, "golist")); err != nil { // go list's own status, as copied out, before the listing is read (Opus #412 B3)
		return nil, err
	}
	if err := checkOut(out, x, held); err != nil {
		return nil, err
	}
	return readListing(out, x)
}

// errRefused marks a listing's failure the candidate caused, on positive
// evidence (boxList): a block, never ErrGuardRun.
var errRefused = errors.New("the listing failed on the candidate")

// refused is err, the candidate's (errRefused).
func refused(err error) error { return fmt.Errorf("%w: %w", errRefused, err) }

// workloadFailed reports whether err, a sandbox run's, is evidence against
// the candidate: a bound its run broke (sandbox.ErrLeft), measured
// (never with ErrUnmeasured: Opus #412 B1), and never with its container's
// removal failing (sandbox.ErrCleanup), which is the controller's whatever
// the run did. The run's own exit status (sandbox.WorkloadExit) never is:
// go list's own status is the script's $r/golist (goListStatus); the
// script's other failures, and a signal, are the host's (Opus #412 B2, B3).
func workloadFailed(err error) bool {
	return errors.Is(err, sandbox.ErrLeft) && !errors.Is(err, sandbox.ErrUnmeasured) && !errors.Is(err, sandbox.ErrCleanup)
}

// goListStatus is the listing's verdict from go list's own status, the
// file at p the script wrote (a few bytes, read bounded): nil for 0; the
// candidate's (errRefused, goListOwn) for go list's own exit 1-124; the
// host's (ErrGuardRun) for a signal (128 and above), a missing or
// malformed file, anything else (Opus #412 B2, B3).
func goListStatus(p string) error {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return errors.Join(err, fmt.Errorf("go list's status is no regular file"))
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 16))
	if err != nil {
		return err
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return fmt.Errorf("go list's status %.16q: %w", b, err)
	}
	return goListExit(code, fmt.Errorf("go list exited %d", code))
}

// killedStep reports whether list, a listing's output, records a step
// inside go list killed by a signal (the compiler OOM-killed under the
// container's memory limit): the host's, never the candidate's (Opus #412
// r2 L2). One rule, the sandbox's listing (listKilled) and LocalLister's.
func killedStep(list []byte) bool { return bytes.Contains(list, []byte("signal: killed")) }

// listKilled is killedStep over the copied-out list at p (read within
// maxList, no symlink followed): ErrGuardRun when it records a killed
// step; an absent list is readListing's to refuse.
func listKilled(p string) error {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return errors.Join(err, errors.New("the list is no regular file"))
	}
	b, err := io.ReadAll(io.LimitReader(f, maxList+1))
	if err != nil {
		return err
	} else if killedStep(b) {
		return fmt.Errorf("%w: a step inside go list was killed by a signal", ErrGuardRun)
	}
	return nil
}

// goListExit is go list's own exit code's verdict (one rule: the sandbox's
// listing, goListStatus, and LocalLister's): nil for 0; the candidate's
// evidence (errRefused) for 1-124, its own error; the host's (ErrGuardRun)
// otherwise, a signal (128 and above) included (Opus #412 B2). Whether the
// evidence is a refusal is listVerdict's: the base's listing must have
// succeeded on the platform (#226 entry 170).
func goListExit(code int, err error) error {
	switch {
	case code == 0:
		return nil
	case code >= 1 && code <= 124:
		return refused(err)
	}
	return fmt.Errorf("%w: %w", ErrGuardRun, err)
}

// readListing reads a listing's version and list, each bounded before it
// is read, the list's records counted and cut, never split, as they are
// parsed (Astra #287 r2 #3, #7).
func readListing(out, x string) (map[string]Listed, error) {
	for name, bound := range map[string]int64{"version": maxVersion, "list": maxList} {
		if st, err := os.Stat(filepath.Join(out, name)); err != nil || st.Size() > bound {
			if err != nil {
				return nil, err
			}
			return nil, refused(fmt.Errorf("a %s over its bound", name))
		}
	}
	v, err := os.ReadFile(filepath.Join(out, "version"))
	if err != nil {
		return nil, err
	} else if v := strings.TrimSpace(string(v)); v != runtime.Version() {
		return nil, fmt.Errorf("the toolchain is %.40q, not the guard's %s", v, runtime.Version())
	}
	list, err := os.ReadFile(filepath.Join(out, "list"))
	if err != nil {
		return nil, err
	}
	got, n := map[string]Listed{}, 0
	for line := range strings.Lines(string(list)) {
		ip, rest, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "\t")
		exp, col, ok2 := strings.Cut(rest, "\t")
		if n++; n > maxListEntries {
			return nil, refused(errors.New("a list over its bound"))
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
