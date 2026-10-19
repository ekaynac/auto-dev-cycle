// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
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
// cache in the run's /tmp.
const listScript = `set -eu
r=$1
shift
go env GOVERSION >"$r/version"
f='{{.ImportPath}}{{"\t"}}{{.Export}}{{"\t"}}{{with .Error}}{{printf "%q" .Err}}{{end}}'
go list -e -export -deps -f "$f" -- "$@" >"$r/list"
if cut -f3 "$r/list" | grep -qF -e 'read-only file system' -e "$GOCACHE"; then
  GOCACHE=/tmp/gocache go list -e -export -deps -f "$f" -- "$@" >"$r/list"
fi
mkdir "$r/x"
awk -F '\t' 'NR == FNR { h[$0] = 1; next } $2 != "" { n = $2; sub(/.*\//, "", n); if (!(n in h)) print $2 }' "$r/have" "$r/list" |
  xargs -r -d '\n' cp -t "$r/x/" --
rm -- "$r/have"
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
	maxListEntries = 1 << 14
	exportName     = regexp.MustCompile(`^[0-9a-f]{64}-d$`)
)

// boxList lists in the sandbox. What comes out is checked for its form,
// its bounds and its consistency, not its provenance: the pinned image and
// go.sum are trusted. Every listing's copy is charged to the run's ledger,
// kept or not; past maxRunCopy, a listing fails.
func boxList(ctx context.Context, h box, img sandbox.Image, lim sandbox.Limits, dir string, pl [2]string, gomod, gosum []byte, paths []string) (_ map[string]Listed, err error) {
	if err := listable(gomod, paths); err != nil {
		return nil, err
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
		return nil, errors.New("over the guard run's export data bound")
	}
	held := map[string]bool{}
	for _, e := range have {
		held[e.Name()] = true
	}
	a, err := h.NewAttempt()
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, a.Teardown()) }()
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
		return nil, err
	}
	out, err := os.MkdirTemp(dir, "list-")
	if err != nil {
		return nil, err
	}
	out = filepath.Join(out, "r")
	err = a.CopyOut(out, maxRunCopy()-charged)
	_ = filepath.WalkDir(out, func(_ string, d fs.DirEntry, _ error) error { // charged, whatever comes of it
		if fi, err := d.Info(); err == nil && !d.IsDir() {
			charged += fi.Size()
		}
		return nil
	})
	if err = errors.Join(err, os.WriteFile(ledger, []byte(strconv.FormatInt(charged, 10)), 0o600)); err != nil {
		return nil, err
	}
	if err := checkOut(out, x, held); err != nil {
		return nil, err
	}
	v, err := os.ReadFile(filepath.Join(out, "version"))
	if err != nil {
		return nil, err
	} else if v := strings.TrimSpace(string(v)); v != runtime.Version() {
		return nil, fmt.Errorf("the toolchain is %.40q, not the guard's %s", v, runtime.Version())
	}
	if st, err := os.Stat(filepath.Join(out, "list")); err != nil || st.Size() > maxList { // bounded before it is read
		return nil, errors.Join(err, errors.New("a list over its bound"))
	}
	list, err := os.ReadFile(filepath.Join(out, "list"))
	if err != nil {
		return nil, err
	} else if bytes.Count(list, []byte("\n")) > maxListEntries {
		return nil, errors.New("a list over its bound")
	}
	got := map[string]Listed{}
	for line := range strings.Lines(string(list)) {
		f := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		if len(f) != 3 {
			return nil, errors.New("an unreadable list")
		}
		l, why := Listed{ImportPath: f[0]}, ""
		if f[2] != "" {
			if why, err = strconv.Unquote(f[2]); err != nil {
				return nil, errors.New("an unreadable list")
			}
		}
		switch e := path.Base(f[1]); {
		case why != "": // an error, whatever else the entry holds (Astra #287 H2)
		case got[f[0]].ImportPath != "":
			why = "listed twice"
		case f[1] != "" && (!exportName.MatchString(e) || !regular(filepath.Join(x, e))):
			why = "its export data not copied out"
		case f[1] != "":
			l.Export = filepath.Join(x, e) // the run's copy of that name, never the path go listed
		}
		if why != "" {
			l.Error = &struct{ Err string }{why}
		}
		got[f[0]] = l
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
		if !slices.Contains([]string{"list", "version", "x"}, e.Name()) {
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
