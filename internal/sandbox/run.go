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
	"syscall"
	"time"
)

// ErrSpec marks a run that is refused before anything starts.
var ErrSpec = errors.New("sandbox: run refused")

// Limits bound one run.
type Limits struct {
	Memory  int64         // bytes (podman --memory)
	CPUs    string        // podman --cpus, e.g. "2"
	PIDs    int           // podman --pids-limit
	Timeout time.Duration // the run is removed after it
}

// Run is one container: its image, mode, attempt, gateway and command.
type Run struct {
	Image   Image
	Mode    Mode
	Attempt *Attempt // its workspace and results are the only directories mounted
	Gateway string   // the gateway's socket on the host: build mode only
	Limits  Limits
	Argv    []string
}

// resolve is p with every symlink resolved; a missing tail is kept as is.
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if d := filepath.Dir(p); d != p {
		return filepath.Join(resolve(d), filepath.Base(p))
	}
	return p
}

// mounts are r's mount sources, resolved: the attempt's workspace and
// results, which must be one attempt's directly under the sandbox root's
// runs/, and in build mode the gateway, which must be a socket. None may
// overlap a controller or Podman path, or be or contain the home.
func (h Host) mounts(r Run) ([]string, error) {
	refuse := func(why string) ([]string, error) { return nil, fmt.Errorf("%w: %s", ErrSpec, why) }
	if r.Attempt == nil || (r.Mode == Gate) != (r.Gateway == "") || r.Mode != Gate && r.Mode != Build {
		return refuse("a run needs an attempt; a gate run has no gateway socket, a build run exactly one")
	}
	ws, res := resolve(r.Attempt.Workspace()), resolve(r.Attempt.Results())
	dir := filepath.Join(resolve(h.Root), "runs", filepath.Base(r.Attempt.Dir))
	if ws != filepath.Join(dir, "workspace") || res != filepath.Join(dir, "results") {
		return refuse("the workspace and results are not one attempt's under the sandbox root")
	}
	mounts := []string{ws, res}
	if r.Mode == Build {
		if st, err := os.Lstat(r.Gateway); err != nil || st.Mode().Type() != fs.ModeSocket || !absClean(r.Gateway) {
			return refuse("the gateway is not a socket")
		}
		mounts = append(mounts, resolve(r.Gateway))
	}
	home := resolve(h.Home)
	for _, m := range mounts {
		if strings.ContainsAny(m, ",\n") || m == home || strings.HasPrefix(home, m+"/") { // a comma would add --mount options
			return refuse("a mount has a comma, or is or contains the home")
		}
		for _, f := range append(h.podmanDirs(), h.Forbidden...) {
			if overlaps(m, resolve(f)) {
				return refuse("a mount overlaps a controller or Podman path")
			}
		}
	}
	return mounts, nil
}

// args are podman's arguments for r, or a refusal.
func (h Host) args(r Run, name string) ([]string, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	m, err := h.mounts(r)
	if err != nil {
		return nil, err
	}
	l := r.Limits
	if !imgID.MatchString(r.Image.ID) || len(r.Argv) == 0 || l.Memory <= 0 || l.CPUs == "" || l.PIDs <= 0 || l.Timeout <= 0 {
		return nil, fmt.Errorf("%w: no resolved image, command or limits", ErrSpec)
	}
	args := []string{"run", "--rm", "--name=" + name, "--pull=never", "--network=none", "--no-hosts", "--hostname=sandbox",
		"--cap-drop=all", "--security-opt=no-new-privileges", "--read-only", "--read-only-tmpfs=false",
		"--systemd=false", "--env-host=false", "--http-proxy=false", "--log-driver=none",
		"--memory=" + strconv.FormatInt(l.Memory, 10), "--cpus=" + l.CPUs, "--pids-limit=" + strconv.Itoa(l.PIDs),
		"--tmpfs=/tmp:rw,nosuid,nodev,size=256m", "--tmpfs=" + homeDir + ":rw,nosuid,nodev,mode=0700,size=64m",
		"--env=HOME=" + homeDir, "--workdir=" + WorkspaceDir,
		"--mount=type=bind,src=" + m[0] + ",dst=" + WorkspaceDir, "--mount=type=bind,src=" + m[1] + ",dst=" + ResultsDir}
	if r.Mode == Build {
		args = append(args, "--mount=type=bind,src="+m[2]+",dst="+GatewaySocket)
	}
	if h.mutate != nil { // tests only: the live fault injections
		args = h.mutate(args)
	}
	return append(append(args, r.Image.ID), r.Argv...), nil
}

// Start runs r to its end, or removes it at its timeout, and returns its
// stdout. --rm removes the container; the workspace and results stay on
// the controller's side for copy-out. A removal that fails marks the
// attempt unsafe: a container may still be writing it.
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
		rm, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, rmErr := h.podman(rm, "rm", "--force", "--ignore", "--time=0", name)
		r.Attempt.unsafe = rmErr != nil
		err = errors.Join(err, run.Err(), rmErr)
	}
	return out, err
}

// Attempt is one run's directories under the sandbox root's runs/.
type Attempt struct {
	Dir    string
	unsafe bool
}

// NewAttempt makes a private directory for one run.
func (h Host) NewAttempt() (*Attempt, error) {
	if err := os.MkdirAll(filepath.Join(h.Root, "runs"), 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(filepath.Join(h.Root, "runs"), "a")
	if err != nil {
		return nil, err
	}
	a := &Attempt{Dir: dir}
	for _, d := range []string{a.Workspace(), a.Results()} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return nil, errors.Join(err, a.Teardown())
		}
	}
	return a, nil
}

func (a *Attempt) Workspace() string { return filepath.Join(a.Dir, "workspace") }
func (a *Attempt) Results() string   { return filepath.Join(a.Dir, "results") }

// CopyOut copies the results' regular files to dst before teardown.
func (a *Attempt) CopyOut(dst string, maxBytes int64) error {
	return a.copy(a.Results(), dst, maxBytes, nil)
}

// CopyTree copies the workspace's tree, symlinks as symlinks, without any
// .git, to dst: the work tree a Repo's git may read.
func (a *Attempt) CopyTree(dst string, maxBytes int64) error {
	return a.copy(a.Workspace(), dst, maxBytes, func(name string) bool { return name == ".git" })
}

// copy copies src to dst, which must not exist, at most maxBytes. A run's files are read only
// through src's root; devices, sockets and FIFOs are never opened, and
// symlinks never followed (copied as links only for a tree).
func (a *Attempt) copy(src, dst string, maxBytes int64, skip func(string) bool) error {
	if a.unsafe {
		return fmt.Errorf("%w: the run's container may still be running", ErrSpec)
	}
	root, err := os.OpenRoot(src)
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
		case skip != nil && skip(d.Name()):
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		case d.IsDir():
			return os.Mkdir(filepath.Join(dst, p), 0o700)
		case d.Type() == fs.ModeSymlink && skip != nil:
			target, err := root.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(target, filepath.Join(dst, p))
		case !d.Type().IsRegular():
			return nil
		}
		in, err := root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer in.Close()
		if st, err := in.Stat(); err != nil || !st.Mode().IsRegular() {
			return fmt.Errorf("%w: %s changed type", ErrSpec, p)
		}
		out, err := os.OpenFile(filepath.Join(dst, p), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		n, err := io.Copy(out, io.LimitReader(in, budget+1))
		if budget -= n; err == nil && budget < 0 {
			err = fmt.Errorf("%w: more than %d bytes", ErrSpec, maxBytes)
		}
		return errors.Join(err, out.Close())
	})
}

// Teardown removes the attempt's directories, first making every
// directory a run left writable again.
func (a *Attempt) Teardown() error {
	_ = filepath.WalkDir(a.Dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(a.Dir)
}
