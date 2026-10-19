// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
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

// goImage is the prepared image's layout (fixtures/m1/images/go/Containerfile).
var goImage = []string{"PATH=/usr/local/go/bin:/usr/bin:/bin", "HOME=/tmp", "GOPATH=/go", "GOCACHE=/opt/adc/gocache"}

// listScript lists "$@" into $1/list, a package a line (its path, export
// data file and quoted error), copies the export data into $1/x and writes
// go's version into $1/version. An error listed with the image's read-only
// build cache (a miss: a platform or package it was not warmed for) lists
// again with a cold cache in the run's /tmp.
const listScript = `set -eu
r=$1
shift
go env GOVERSION >"$r/version"
f='{{.ImportPath}}{{"\t"}}{{.Export}}{{"\t"}}{{with .Error}}{{printf "%q" .Err}}{{end}}'
go list -e -export -deps -f "$f" -- "$@" >"$r/list"
if cut -f3 "$r/list" | grep -q .; then GOCACHE=/tmp/gocache go list -e -export -deps -f "$f" -- "$@" >"$r/list"; fi
mkdir "$r/x"
cut -f2 "$r/list" | while read -r e; do if [ -n "$e" ]; then cp -- "$e" "$r/x/"; fi; done
`

// SandboxLister is the production Lister (operator answer "loader b"): go
// list -export runs in img, the prepared Go image, through h's sandbox in
// gate mode (no network; only the run's workspace and results mounted),
// under GoEnv, with only go.mod and go.sum in its workspace. Only the list
// and the export data come out, into the guard run's directory. go's
// release must be the guard's (one export data format); lim bounds each run.
func SandboxLister(h sandbox.Host, img sandbox.Image, lim sandbox.Limits) Lister {
	return boxLister(h, img, lim)
}

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

func boxList(ctx context.Context, h box, img sandbox.Image, lim sandbox.Limits, dir string, pl [2]string, gomod, gosum []byte, paths []string) (_ map[string]Listed, err error) {
	if err := listable(gomod, paths); err != nil {
		return nil, err
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
	if err := a.CopyOut(filepath.Join(out, "r"), maxExport+maxList); err != nil {
		return nil, err
	}
	out = filepath.Join(out, "r")
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
	got := map[string]Listed{}
	for line := range strings.Lines(string(list)) {
		f := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		if len(f) != 3 {
			return nil, errors.New("an unreadable list")
		}
		l := Listed{ImportPath: f[0]}
		if f[2] != "" {
			why, err := strconv.Unquote(f[2])
			if err != nil {
				return nil, errors.New("an unreadable list")
			}
			l.Error = &struct{ Err string }{why}
		}
		if x := filepath.Join(out, "x", path.Base(f[1])); f[1] != "" { // the copy of that name, never the path go listed
			if st, err := os.Lstat(x); err == nil && st.Mode().IsRegular() {
				l.Export = x
			}
		}
		if f[1] != "" && l.Export == "" && l.Error == nil {
			l.Error = &struct{ Err string }{"its export data not copied out"}
		}
		got[l.ImportPath] = l
	}
	return got, nil
}
