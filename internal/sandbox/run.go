// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrSpec marks a run that is refused before anything starts.
var ErrSpec = errors.New("sandbox: run refused")

// Limits bound one run.
type Limits struct {
	Memory  string        // podman --memory, e.g. "2g"
	CPUs    string        // podman --cpus, e.g. "2"
	PIDs    int           // podman --pids-limit
	Timeout time.Duration // the run is killed after it
}

// Run is one container: its image, mode, mounts and command.
type Run struct {
	Image     Image
	Mode      Mode
	Workspace string // controller-side directory, mounted at WorkspaceDir
	Results   string // controller-owned directory, mounted at ResultsDir
	Gateway   string // the gateway's socket on the host: build mode only
	Limits    Limits
	Argv      []string
}

// args are podman's arguments for r, or a refusal. Each mount is checked
// against the forbidden paths and the sandbox root.
func (h Host) args(r Run, name string) ([]string, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	mounts := []string{r.Workspace, r.Results}
	switch {
	case r.Mode == Gate && r.Gateway == "":
	case r.Mode == Build && r.Gateway != "":
		mounts = append(mounts, r.Gateway)
	default:
		return nil, fmt.Errorf("%w: a gate run has no gateway socket, a build run exactly one", ErrSpec)
	}
	for _, m := range mounts {
		if !absClean(m) || strings.ContainsAny(m, ",\n") { // a comma would add --mount options
			return nil, fmt.Errorf("%w: a mount is not an absolute, clean path without a comma", ErrSpec)
		}
		for _, f := range append(h.podmanDirs(), h.Forbidden...) {
			if overlaps(m, f) {
				return nil, fmt.Errorf("%w: a mount overlaps a controller or Podman path", ErrSpec)
			}
		}
	}
	l := r.Limits
	if !imgID.MatchString(r.Image.ID) || len(r.Argv) == 0 || l.Memory == "" || l.CPUs == "" || l.PIDs <= 0 || l.Timeout <= 0 {
		return nil, fmt.Errorf("%w: no resolved image, command or limits", ErrSpec)
	}
	args := []string{"run", "--rm", "--name=" + name, "--pull=never", "--network=none", "--no-hosts", "--hostname=sandbox",
		"--cap-drop=all", "--security-opt=no-new-privileges", "--read-only", "--read-only-tmpfs=false",
		"--systemd=false", "--env-host=false", "--http-proxy=false", "--log-driver=none",
		"--memory=" + l.Memory, "--cpus=" + l.CPUs, "--pids-limit=" + strconv.Itoa(l.PIDs),
		"--tmpfs=/tmp:rw,nosuid,nodev,size=256m", "--tmpfs=" + homeDir + ":rw,nosuid,nodev,mode=0700,size=64m",
		"--env=HOME=" + homeDir, "--workdir=" + WorkspaceDir,
		"--mount=type=bind,src=" + r.Workspace + ",dst=" + WorkspaceDir,
		"--mount=type=bind,src=" + r.Results + ",dst=" + ResultsDir}
	if r.Mode == Build {
		args = append(args, "--mount=type=bind,src="+r.Gateway+",dst="+GatewaySocket)
	}
	if h.mutate != nil { // tests only: the live fault injections
		args = h.mutate(args)
	}
	args = append(args, r.Image.ID)
	return append(args, r.Argv...), nil
}

// Start runs r to its end, or removes it at its timeout, and returns its
// stdout. --rm removes the container; the workspace and results stay on
// the controller's side for copy-out (Attempt).
func (h Host) Start(ctx context.Context, r Run) ([]byte, error) {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	name := "adc-" + hex.EncodeToString(b)
	args, err := h.args(r, name)
	if err != nil {
		return nil, err
	}
	run, cancel := context.WithTimeout(ctx, r.Limits.Timeout)
	defer cancel()
	out, err := h.podman(run, args...)
	if run.Err() != nil {
		// Killing the client does not stop the container; removing it does.
		_, rmErr := h.podman(context.WithoutCancel(ctx), "rm", "--force", "--ignore", "--time=0", name)
		err = errors.Join(err, run.Err(), rmErr)
	}
	return out, err
}

// Attempt is one run's directories under the sandbox root: a workspace,
// a results directory, and the copy-out taken before teardown.
type Attempt struct{ Dir string }

// NewAttempt makes a private directory for one run.
func (h Host) NewAttempt() (Attempt, error) {
	if err := os.MkdirAll(filepath.Join(h.Root, "runs"), 0o700); err != nil {
		return Attempt{}, err
	}
	dir, err := os.MkdirTemp(filepath.Join(h.Root, "runs"), "a")
	if err != nil {
		return Attempt{}, err
	}
	a := Attempt{Dir: dir}
	for _, d := range []string{a.Workspace(), a.Results()} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return Attempt{}, errors.Join(err, a.Teardown())
		}
	}
	return a, nil
}

func (a Attempt) Workspace() string { return filepath.Join(a.Dir, "workspace") }
func (a Attempt) Results() string   { return filepath.Join(a.Dir, "results") }

// CopyOut copies the results' regular files, at most maxBytes in all, to
// dst, which must not exist. Symlinks, devices, sockets and FIFOs a run
// left are never followed or copied; the run's files are read only
// through the results root.
func (a Attempt) CopyOut(dst string, maxBytes int64) error {
	root, err := os.OpenRoot(a.Results())
	if err != nil {
		return err
	}
	defer root.Close()
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	budget := maxBytes
	return fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case p == ".":
			return nil
		case d.IsDir():
			return os.Mkdir(filepath.Join(dst, p), 0o700)
		case !d.Type().IsRegular():
			return nil
		}
		in, err := root.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(filepath.Join(dst, p), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		n, err := io.Copy(out, io.LimitReader(in, budget+1))
		if budget -= n; err == nil && budget < 0 {
			err = fmt.Errorf("%w: the results exceed %d bytes", ErrSpec, maxBytes)
		}
		return errors.Join(err, out.Close())
	})
}

// Teardown removes the attempt's directories.
func (a Attempt) Teardown() error { return os.RemoveAll(a.Dir) }
