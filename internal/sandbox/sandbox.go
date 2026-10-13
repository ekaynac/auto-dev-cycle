// SPDX-License-Identifier: Apache-2.0

// Package sandbox is sandbox phase 2 (docs/design/m1/00 §1, P9; Q1): one
// rootless Podman container per run, from a toolchain image pinned by
// digest. The container gets a fresh workspace and a results directory the
// controller owns, never a credential, a controller path or a network: in
// gate mode it reaches nothing, in build mode only the gateway socket.
// Podman itself runs under an empty environment (env -i) with HOME, XDG_*,
// storage and runtime directories inside Host.Root, so the user's own
// Podman state, sockets and configuration are never read or written.
// Rootless, the container's root is the controller's uid on the host, so
// what is mounted is the boundary: Run refuses any mount that overlaps a
// controller or Podman path, and the probe checks what a run can reach.
//
// bubblewrap stays the test sandbox (scripts/test-sandbox.sh); this is the
// product's.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Mode is what a run may reach.
type Mode string

const (
	Gate  Mode = "gate"  // no network, no socket
	Build Mode = "build" // no network; the gateway socket only
)

// Paths inside the container.
const (
	WorkspaceDir  = "/workspace"
	ResultsDir    = "/results"
	GatewaySocket = "/run/adc/gateway.sock"
	homeDir       = "/home/adc"
)

// maxRunRoot is Podman's limit on the runroot's length.
const maxRunRoot = 50

// ErrHost marks a Host that is refused. Messages are fixed text.
var ErrHost = errors.New("sandbox: host configuration")

// Host is how the controller runs Podman.
type Host struct {
	Podman string // absolute path of the podman binary
	// Root is a private directory of the controller's: Podman's HOME,
	// XDG_*, storage (images pulled by digest beforehand) and runroot,
	// and each run's directories. It must be short (Podman's runroot).
	Root string
	// Forbidden are controller paths a run never mounts and must not
	// see: the state directory, adcd's private and shared sockets, the
	// Podman API socket.
	Forbidden []string
	Runner    Runner // nil runs the command
	mutate    func([]string) []string
}

// Runner runs one command with exactly env and returns its stdout.
type Runner interface {
	Run(ctx context.Context, name string, args, env []string) ([]byte, error)
}

// Validate checks the host's paths.
func (h Host) Validate() error {
	switch {
	case !absClean(h.Podman) || !absClean(h.Root):
		return fmt.Errorf("%w: podman and the sandbox root must be absolute, clean paths", ErrHost)
	case len(h.runRoot()) > maxRunRoot:
		return fmt.Errorf("%w: the sandbox root is too long for Podman's runroot", ErrHost)
	}
	for _, f := range h.Forbidden {
		if !absClean(f) || overlaps(f, h.Root) {
			return fmt.Errorf("%w: a forbidden path is not absolute and clean, or overlaps the sandbox root", ErrHost)
		}
	}
	return nil
}

func (h Host) runRoot() string { return filepath.Join(h.Root, "r") }

// podmanDirs are Podman's own directories under the root: never mounted.
func (h Host) podmanDirs() []string {
	return []string{filepath.Join(h.Root, "home"), filepath.Join(h.Root, "xdg"), filepath.Join(h.Root, "storage"), h.runRoot()}
}

// env is Podman's whole environment: nothing of the controller's.
func (h Host) env() []string {
	home := filepath.Join(h.Root, "home")
	return []string{"HOME=" + home, "PATH=/usr/bin:/bin", "XDG_RUNTIME_DIR=" + filepath.Join(h.Root, "xdg"),
		"XDG_CONFIG_HOME=" + home + "/.config", "XDG_DATA_HOME=" + home + "/.local/share",
		"XDG_STATE_HOME=" + home + "/.local/state", "XDG_CACHE_HOME=" + home + "/.cache"}
}

// podman runs podman with the host's global flags.
func (h Host) podman(ctx context.Context, args ...string) ([]byte, error) {
	global := []string{"--root", filepath.Join(h.Root, "storage"), "--runroot", h.runRoot(),
		"--cgroup-manager=cgroupfs", "--events-backend=none"}
	for _, d := range h.podmanDirs()[:2] { // HOME and XDG_RUNTIME_DIR
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	r := h.Runner
	if r == nil {
		r = execRunner{}
	}
	return r.Run(ctx, h.Podman, append(global, args...), h.env())
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env, cmd.Dir = env, "/"
	stdout, stderr := &limited{n: maxStdout}, &limited{n: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(stderr.b.String()))
	}
	if stdout.over {
		return nil, fmt.Errorf("%s: its output exceeds %d bytes", filepath.Base(name), maxStdout)
	}
	return stdout.b.Bytes(), nil
}

const maxStdout = 1 << 20

// limited keeps the first n bytes written, and notes any beyond. The
// buffer is not embedded: its ReadFrom would bypass Write.
type limited struct {
	b    bytes.Buffer
	n    int
	over bool
}

func (l *limited) Write(p []byte) (int, error) {
	room := max(l.n-l.b.Len(), 0)
	l.b.Write(p[:min(room, len(p))])
	l.over = l.over || len(p) > room
	return len(p), nil
}

func absClean(p string) bool { return filepath.IsAbs(p) && filepath.Clean(p) == p && p != "/" }

// overlaps: a is b, or one contains the other.
func overlaps(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
