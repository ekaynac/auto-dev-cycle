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
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrSpec marks a run that is refused before anything starts.
var ErrSpec = errors.New("sandbox: run refused")

// ErrLeft marks a run that ran and left more than its bounds, or what
// cannot be measured (within): what it did, not a refusal to start it.
var ErrLeft = errors.New("sandbox: the run left more than its bounds")

// ErrBound marks a copy-out or walk refused at its bound (bytes, entries,
// levels): what the run wrote is past it.
var ErrBound = errors.New("sandbox: past its bound")

// ErrCleanup marks the removal of a run's container failing after the run
// (podman rm): the controller's, reported apart from the run's own end.
var ErrCleanup = errors.New("sandbox: the run's container was not removed")

// WorkloadExit is a run's workload's own exit status, observed (Lead
// decision, #226 entry 147): podman run returned it as its own status,
// 1-124, or 128 and above for a signal the workload's process got, within
// the run's timeout. The client's own termination (killed: -1; podman's
// failures: 125-127), a run cut at its timeout, and cleanup are never one.
// Err is podman's exit error.
type WorkloadExit struct {
	Code int
	Err  error
}

func (e *WorkloadExit) Error() string { return fmt.Sprintf("sandbox: the workload exited %d", e.Code) }
func (e *WorkloadExit) Unwrap() error { return e.Err }

// workload is err, podman run's, as a WorkloadExit when it is one (cut:
// the run's timeout ended it).
func workload(err error, cut bool) error {
	var exit *exec.ExitError
	if cut || !errors.As(err, &exit) {
		return err
	}
	if c := exit.ExitCode(); c >= 1 && c <= 124 || c >= 128 && c <= 255 {
		return &WorkloadExit{Code: c, Err: err}
	}
	return err
}

// ErrUnmeasured marks, with ErrLeft, a tree that cannot be measured for a
// cause that is not the candidate's (an I/O error, a missing directory, the
// exchange the release's adapter wrote): no breach is known (Mythos chunk 2
// L3). What the candidate made unreadable (a permission error under the
// workspace) stays ErrLeft alone, its own.
var ErrUnmeasured = errors.New("sandbox: the run's tree cannot be measured")

// Limits bound one run.
type Limits struct {
	Memory  int64         // bytes (podman --memory)
	CPUs    string        // podman --cpus, e.g. "2"
	PIDs    int           // podman --pids-limit
	Timeout time.Duration // the run is removed after it
	Disk    int64         // bytes the attempt may hold once the run ends
}

// conmonGrace is how long after a run's Limits.Timeout conmon ends it on
// its own: the controller's deadline always fires first.
const conmonGrace = 30 * time.Second

// Run is one container: its image, mode, attempt, gateway and command.
type Run struct {
	Image   Image
	Mode    Mode
	Attempt *Attempt // its workspace and results are the only directories mounted
	Gateway string   // the gateway's socket on the host: build mode only
	// Exchange is an adapter's exchange root on the host (in/ and out/,
	// P22a-5d), mounted at ExchangeIn (read-only) and ExchangeOut: an
	// interactive build run's only (Host.Interactive), under
	// Host.ExchangeRoot, named exchange-<its attempt>.
	Exchange string
	Limits   Limits
	Argv     []string
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
// runs/, in build mode the gateway, which must be a socket, and an
// interactive run's exchange in/ and out/, real directories of a root
// directly under ExchangeRoot. None may overlap a controller or Podman
// path, or be or contain the home.
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
	if r.Exchange != "" {
		x := filepath.Join(resolve(h.ExchangeRoot()), filepath.Base(r.Exchange))
		if r.Mode != Build || !absClean(r.Exchange) || resolve(r.Exchange) != x || !strings.HasPrefix(filepath.Base(x), "exchange-") {
			return refuse("an exchange is a build run's, a root directly under the sandbox's exchange root")
		}
		for _, d := range []string{"in", "out"} {
			if st, err := os.Lstat(filepath.Join(x, d)); err != nil || !st.IsDir() {
				return refuse("an exchange's in/ and out/ are directories, never symlinks")
			}
			mounts = append(mounts, filepath.Join(x, d))
		}
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
	// --timeout: conmon itself ends the run, even with no controller left
	// to kill the client (a crash; #374 M3). It is that backstop, not the
	// deadline: the controller's (the session's, Start's) comes first, by
	// conmonGrace (#374 Mythos L1).
	args := []string{"run", "--rm", "--name=" + name, "--pull=never", "--network=none", "--no-hosts", "--hostname=sandbox",
		"--user=0:0", "--cap-drop=all", "--security-opt=no-new-privileges", "--read-only", "--read-only-tmpfs=false",
		"--systemd=false", "--env-host=false", "--http-proxy=false", "--log-driver=none",
		"--memory=" + strconv.FormatInt(l.Memory, 10), "--cpus=" + l.CPUs, "--pids-limit=" + strconv.Itoa(l.PIDs),
		"--timeout=" + strconv.FormatInt(int64(math.Ceil(l.Timeout.Seconds()))+int64(conmonGrace/time.Second), 10),
		"--tmpfs=/tmp:rw,nosuid,nodev,size=256m", "--tmpfs=" + homeDir + ":rw,nosuid,nodev,mode=0700,size=64m",
		"--env=HOME=" + homeDir, "--workdir=" + WorkspaceDir,
		"--mount=type=bind,src=" + m[0] + ",dst=" + WorkspaceDir, "--mount=type=bind,src=" + m[1] + ",dst=" + ResultsDir}
	if r.Mode == Build {
		args = append(args, "--mount=type=bind,src="+m[2]+",dst="+GatewaySocket)
	}
	if r.Exchange != "" {
		// An interactive run: the adapter's stdio (no detach keys: its input
		// is the session's, whatever the bytes), the container's id in the
		// attempt's directory (Interactive's removal is by that id, never by
		// a name another run may hold), its exchange, and where its gateway is.
		args = append(args, "--interactive", "--detach-keys=", "--cidfile="+cidFile(filepath.Dir(m[0])),
			"--mount=type=bind,src="+m[3]+",dst="+ExchangeIn+",ro=true",
			"--mount=type=bind,src="+m[4]+",dst="+ExchangeOut, "--env=ADC_GATEWAY_SOCKET="+GatewaySocket)
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
	if err == nil && r.Exchange != "" {
		err = fmt.Errorf("%w: an exchange is an interactive run's", ErrSpec)
	}
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
	if rmErr != nil { // reported apart: never the workload's status
		rmErr = fmt.Errorf("%w: %w", ErrCleanup, rmErr)
	}
	// What a run left is bounded whatever its outcome: a failed or
	// timed-out run fills runs/ as well as a clean one.
	return out, errors.Join(workload(err, run.Err() != nil), run.Err(), rmErr, r.Attempt.within(r.Limits.Disk, 0))
}

// within refuses an attempt that holds more than disk bytes, maxEntries
// entries or maxDepth levels, or one that cannot be measured, a missing
// directory among them. also are directories the run wrote besides the
// attempt's (an interactive run's exchange out/, measured before its host
// removes it), counted against the same bound with it, each within
// outLimit entries.
func (a *Attempt) within(disk int64, outLimit int, also ...string) error {
	var size int64
	var err error
	at := 0 // the directory err is from: 0 the attempt's
	for i, dir := range append([]string{a.Dir}, also...) {
		at = i
		var root *os.Root
		if root, err = os.OpenRoot(dir); err != nil {
			break
		}
		limit := maxEntries
		if i > 0 {
			limit = outLimit
		}
		err = walk(root.FS(), ".", limit, func(p string, d fs.DirEntry) error {
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
		root.Close()
		if err != nil {
			break
		}
	}
	if err != nil && !errors.Is(err, ErrSpec) {
		if !candidates(at, err) {
			err = errors.Join(ErrUnmeasured, err)
		}
		err = fmt.Errorf("%w: the run left what cannot be measured: %w", ErrSpec, err)
	}
	if err != nil {
		err = errors.Join(ErrLeft, err)
	}
	return err
}

// candidates reports whether err, from walking directory at of within's,
// is one the candidate made: a permission error below the attempt's
// workspace, which the candidate writes (a 000 directory in /workspace).
func candidates(at int, err error) bool {
	var pe *fs.PathError
	return at == 0 && errors.Is(err, fs.ErrPermission) && errors.As(err, &pe) &&
		(pe.Path == "workspace" || strings.HasPrefix(pe.Path, "workspace/"))
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
					return fmt.Errorf("%w: %w: more than %d entries", ErrSpec, ErrBound, limit)
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
	a := &Attempt{Dir: dir, unshare: h.unshareRemove}
	for _, d := range []string{a.Workspace(), a.Results()} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return nil, errors.Join(err, a.Teardown())
		}
	}
	return a, nil
}

// unshareRemove removes dir in the rootless user namespace: what a run's
// other uids left.
func (h Host) unshareRemove(dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := h.podman(ctx, "unshare", "rm", "-rf", "--", dir)
	return err
}

// Runs are the run directories under the root's runs/, by path.
func (h Host) Runs() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(h.Root, "runs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && e.Type()&fs.ModeSymlink == 0 {
			dirs = append(dirs, filepath.Join(h.Root, "runs", e.Name()))
		}
	}
	return dirs, err
}

// Kept is dir, a run directory under the root's runs/ that an earlier run
// kept (its container's end unverified), as an Attempt to tear down only:
// the start sweep's, once that container is removed. Nothing is copied out
// of it.
func (h Host) Kept(dir string) (*Attempt, error) {
	if filepath.Dir(dir) != filepath.Join(h.Root, "runs") {
		return nil, fmt.Errorf("%w: %s is no run directory", ErrSpec, dir)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is no run directory", ErrSpec, dir)
	}
	return &Attempt{Dir: dir, unsafe: true, unshare: h.unshareRemove}, nil
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

// MaxOutEntries is what one cleanup of an adapter's host visits over its
// whole exchange (adapterhost.MaxWork, which a daemon row pins equal). An
// interactive run's out/ may hold that less its exchange's other entries
// (the root's exchangeOwn and in/'s), so the exchange is never left half
// removed (Opus 22a-5d-4 L2; Mythos chunk 2 L2). maxOutEntries is it;
// tests lower it.
const MaxOutEntries = 1 << 16

var maxOutEntries = MaxOutEntries

// exchangeOwn are the entries of an exchange its cleanup visits besides
// in/'s and out/'s: the root, in/ and out/ themselves.
const exchangeOwn = 3

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
			return fmt.Errorf("%w: %w: more than %d levels", ErrSpec, ErrBound, maxDepth)
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
			err = fmt.Errorf("%w: %w: more than %d bytes", ErrSpec, ErrBound, maxBytes)
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
