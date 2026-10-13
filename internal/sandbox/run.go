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
	"math"
	"os"
	"path"
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
	Disk    int64         // bytes the attempt may hold once the run ends
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
	dir := filepath.Join(resolve(filepath.Join(h.Root, "runs")), filepath.Base(r.Attempt.Dir)) // runs/ may be the operator's mount or link
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
	if !imgID.MatchString(r.Image.ID) || len(r.Argv) == 0 || l.Memory <= 0 || l.CPUs == "" || l.PIDs <= 0 || l.Timeout <= 0 || l.Disk <= 0 {
		return nil, fmt.Errorf("%w: no resolved image, command or limits", ErrSpec)
	}
	args := []string{"run", "--rm", "--name=" + name, "--pull=never", "--network=none", "--no-hosts", "--hostname=sandbox",
		"--user=0:0", "--cap-drop=all", "--security-opt=no-new-privileges", "--read-only", "--read-only-tmpfs=false",
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
// stdout. The workspace and results stay on the controller's side for
// copy-out. Whatever the run's outcome (a client killed at the timeout or
// dead for any reason leaves the container running), the container is
// then removed by name; a removal that fails marks the attempt unsafe: a
// container may still be writing it.
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
	rm, cancelRm := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelRm()
	_, rmErr := h.podman(rm, "rm", "--force", "--ignore", "--time=0", name)
	r.Attempt.unsafe = rmErr != nil
	// What a run left is bounded whatever its outcome: a failed or
	// timed-out run fills runs/ as well as a clean one.
	return out, errors.Join(err, run.Err(), rmErr, r.Attempt.within(r.Limits.Disk))
}

// within refuses an attempt that holds more than disk bytes, maxEntries
// entries or maxDepth levels, or one that cannot be measured.
func (a *Attempt) within(disk int64) error {
	root, err := os.OpenRoot(a.Dir)
	if err == nil {
		defer root.Close()
		var size int64
		err = walk(root.FS(), ".", maxEntries, func(p string, d fs.DirEntry) error {
			info, err := d.Info()
			if err != nil {
				return err
			}
			n := info.Size() // or the blocks allocated, if more (fallocate KEEP_SIZE, …)
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Blocks > n/512 {
				n = math.MaxInt64
				if st.Blocks <= math.MaxInt64/512 {
					n = st.Blocks * 512
				}
			}
			if n > disk-size || strings.Count(p, "/") >= maxDepth {
				return fmt.Errorf("%w: the run left more than %d bytes or %d levels", ErrSpec, disk, maxDepth)
			}
			size += n
			return nil
		})
	}
	if err != nil && !errors.Is(err, ErrSpec) {
		err = fmt.Errorf("%w: the run left what cannot be measured: %w", ErrSpec, err)
	}
	return err
}

// walk calls fn for every entry below dir, depth first, reading each
// directory maxBatch entries at a time (a huge directory is never held
// whole) and refusing more than limit entries in all (0: no limit). It descends only into
// directories (never through a symlink); fn returning fs.SkipDir skips one.
func walk(fsys fs.FS, dir string, limit int, fn func(p string, d fs.DirEntry) error) error {
	entries := 0
	var rec func(dir string) error
	rec = func(dir string) error {
		f, err := fsys.Open(dir)
		if err != nil {
			return err
		}
		defer f.Close()
		rd, ok := f.(fs.ReadDirFile)
		if !ok {
			return fmt.Errorf("%w: %s is not a directory", ErrSpec, dir)
		}
		for {
			batch, err := rd.ReadDir(maxBatch)
			for _, d := range batch {
				if entries++; limit > 0 && entries > limit {
					return fmt.Errorf("%w: more than %d entries", ErrSpec, maxEntries)
				}
				p := path.Join(dir, d.Name())
				ferr := fn(p, d)
				if ferr == nil && d.IsDir() {
					ferr = rec(p)
				}
				if ferr != nil && ferr != fs.SkipDir {
					return ferr
				}
			}
			if err == io.EOF {
				return nil
			} else if err != nil {
				return err
			}
		}
	}
	return rec(dir)
}

// Attempt is one run's directories under the sandbox root's runs/.
type Attempt struct {
	Dir     string
	unsafe  bool
	unshare func(dir string) error // removes what a run's other uids left
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
	a := &Attempt{Dir: dir, unshare: func(dir string) error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, err := h.podman(ctx, "unshare", "rm", "-rf", "--", dir)
		return err
	}}
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

var maxEntries, maxDepth, maxBatch = 100_000, 64, 1024 // var: tests lower them

// copy copies src to dst, which must not exist, at most maxBytes,
// maxEntries entries and maxDepth levels. A run's files are read only
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
	return walk(root.FS(), ".", maxEntries, func(p string, d fs.DirEntry) error {
		switch {
		case strings.Count(p, "/") >= maxDepth:
			return fmt.Errorf("%w: more than %d levels", ErrSpec, maxDepth)
		case skip != nil && skip(d.Name()):
			return fs.SkipDir // a .git directory or file: neither is copied
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
		st, err := in.Stat()
		if err != nil || !st.Mode().IsRegular() {
			return fmt.Errorf("%w: %s changed type", ErrSpec, p)
		}
		mode := os.FileMode(0o600)
		if st.Mode()&0o111 != 0 { // git records only the execute bit
			mode = 0o700
		}
		out, err := os.OpenFile(filepath.Join(dst, p), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
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
// directory a run left writable again. What a run's other uids own (an
// image's process that changed user) is removed inside Podman's user
// namespace.
func (a *Attempt) Teardown() error {
	// Batched like every walk here, with no entry cap: teardown must finish
	// what a refused run left. A walk error is RemoveAll's to report.
	_ = os.Chmod(a.Dir, 0o700)
	_ = walk(os.DirFS(a.Dir), ".", 0, func(p string, d fs.DirEntry) error {
		if d.IsDir() {
			_ = os.Chmod(filepath.Join(a.Dir, p), 0o700)
		}
		return nil
	})
	err := os.RemoveAll(a.Dir)
	if err != nil && a.unshare != nil {
		err = errors.Join(a.unshare(a.Dir), os.RemoveAll(a.Dir))
	}
	return err
}
