// SPDX-License-Identifier: Apache-2.0

// Package session is the core's side of one adapter session
// (docs/design/m0/09): the adapter process in its PID namespace, or in a
// container (Container, P22a-5d), bounded by a deadline, with its stdio for
// the method client.
package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session/inproc"
)

// StderrTail is how much of an adapter's stderr a process keeps: the last
// bytes, the rest is dropped as it arrives.
const StderrTail = 64 << 10

// ProcessSpec is the adapter command and its bounds. Neither Command nor Env
// ever appears in an error of this package: secrets reach an adapter through
// its environment, and a command line may carry them too.
type ProcessSpec struct {
	// Command is the adapter's argv; the first word is resolved inside the
	// namespace with Env's PATH.
	Command []string
	// Env is the adapter's whole environment: nothing is inherited.
	Env []string
	// Deadline bounds the process from spawn: past it the namespace is
	// killed.
	Deadline time.Duration
}

// Process is one adapter process in a PID namespace of its own (bubblewrap
// --unshare-pid). Killing the namespace's init ends every process the
// adapter started, whatever sessions or groups they made, and bubblewrap
// exits only after its init is reaped, which the kernel allows only once the
// namespace is empty; Wait also waits for the init's own exit, which the
// kernel reports only once the namespace is empty. So Wait returning means
// the process tree has exited (ingestion relies on it, docs/design/m0/09).
//
// Signals go only through pidfds (one exception, in Spawn's failure path,
// before bubblewrap can be reaped), never a numeric pid or process group: a
// pidfd names one process for good, so a signal can never reach a process
// that reused a pid. Both pidfds are closed, under mu, before Wait returns;
// a Kill after that sends nothing.
//
// This is a lifetime boundary only. Files, network and every other namespace
// are the caller's: it is not a boundary against a hostile adapter, which can
// still ask a service outside the namespace to work for it. That containment
// is a separate, operator-gated change.
type Process struct {
	cmd     *exec.Cmd
	stdin   *os.File
	stdout  *os.File
	expired atomic.Bool
	timer   *time.Timer
	exited  chan struct{}
	waitErr error

	mu     sync.Mutex
	stderr []byte
	// pidfds of bubblewrap and of the namespace's init; -1 once closed (or,
	// for the init, never opened)
	bwrapFD, initFD int
	// killed: Kill was asked (during startup, only by the deadline). The
	// init's pidfd is published under mu: a Kill before it is repeated then.
	killed bool
	// stop, for a peer (startPeer), closes the peer's ends of the pipes.
	stop func()
}

// startPeer runs peer in this process, on its own goroutine, as the adapter
// of a session: in is what the core writes, out what it reads. Kill (the
// deadline among its causes) closes the peer's ends, so its reads and writes
// fail; Wait returns once peer has returned. Nothing contains it: only the
// fixture harness can make a peer (package inproc).
func startPeer(peer *inproc.Peer, deadline time.Duration) (*Process, error) {
	if peer == nil || deadline <= 0 {
		return nil, errors.New("adapterhost: a peer needs a function and a positive deadline")
	}
	var pipes [4]*os.File // stdin, stdout: read end, write end
	for i := 0; i < 4; i += 2 {
		var err error
		if pipes[i], pipes[i+1], err = os.Pipe(); err != nil {
			closeAll(pipes[:i]...)
			return nil, fmt.Errorf("adapterhost: pipe: %w", err)
		}
	}
	p := &Process{stdin: pipes[1], stdout: pipes[2], exited: make(chan struct{}), bwrapFD: -1, initFD: -1,
		stop: func() { closeAll(pipes[0], pipes[3]) }}
	p.timer = time.AfterFunc(deadline, func() {
		p.expired.Store(true)
		p.Kill()
	})
	go func() {
		defer close(p.exited)
		peer.Run(pipes[0], pipes[3])
		p.stop()
	}()
	return p, nil
}

// contain is the bubblewrap command line: a new PID namespace, everything
// else shared, the init's pid written to fd 3.
func contain(command []string) []string {
	return append([]string{"--unshare-pid", "--die-with-parent", "--info-fd", "3",
		"--dev-bind", "/", "/", "--proc", "/proc", "--"}, command...)
}

// Spawn starts the command in its namespace, in a process group of its own.
// The deadline starts now.
func Spawn(spec ProcessSpec) (*Process, error) {
	if len(spec.Command) == 0 || spec.Deadline <= 0 {
		return nil, errors.New("adapterhost: a process needs a command and a positive deadline")
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("%w: bubblewrap: %w", ErrSpawn, err)
	}
	var pipes [8]*os.File // stdin, stdout, stderr, info: read end, write end
	for i := 0; i < 8; i += 2 {
		if pipes[i], pipes[i+1], err = os.Pipe(); err != nil {
			closeAll(pipes[:i]...)
			return nil, fmt.Errorf("adapterhost: pipe: %w", err)
		}
	}
	cmd := exec.Command(bwrap, contain(spec.Command)...)
	cmd.Env = append([]string{}, spec.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pipes[0], pipes[3], pipes[5]
	cmd.ExtraFiles = []*os.File{pipes[7]}
	bwrapFD := -1
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, PidFD: &bwrapFD}
	err = cmd.Start()
	closeAll(pipes[0], pipes[3], pipes[5], pipes[7]) // the child's ends
	if err != nil {
		closeAll(pipes[1], pipes[2], pipes[4], pipes[6])
		// exec's error names bubblewrap's path, never the adapter's argv or env.
		return nil, fmt.Errorf("%w: %w", ErrSpawn, err)
	}
	p := &Process{cmd: cmd, stdin: pipes[1], stdout: pipes[2], exited: make(chan struct{}), bwrapFD: bwrapFD, initFD: -1}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(p, pipes[4]) // ends when the last process holding stderr is gone
		pipes[4].Close()
	}()
	// The waiter starts only once initFD is settled, so it never misses it.
	// Without the init's exit seen through its pidfd, Wait never claims the
	// namespace is empty: bubblewrap can exit before its init does.
	waiter := func() {
		p.waitErr = cmd.Wait()
		p.mu.Lock()
		initFD := p.initFD
		p.mu.Unlock()
		if !verified(initFD) {
			p.waitErr = ErrUnverified
		}
		<-drained
		p.mu.Lock()
		for _, fd := range []*int{&p.bwrapFD, &p.initFD} {
			if *fd >= 0 {
				unix.Close(*fd)
				*fd = -1
			}
		}
		p.mu.Unlock()
		close(p.exited)
	}
	p.timer = time.AfterFunc(spec.Deadline, func() {
		p.expired.Store(true)
		p.Kill()
	})
	var info struct {
		ChildPID int64 `json:"child-pid"`
	}
	err = json.NewDecoder(io.LimitReader(pipes[6], 4096)).Decode(&info)
	pipes[6].Close()
	fd := -1
	if err == nil && info.ChildPID > 0 {
		fd = openInit(int(info.ChildPID), cmd.Process.Pid)
	}
	if startupHook != nil {
		startupHook(p, fd)
	}
	// Published under mu, which Kill takes: a Kill that ran before (the
	// deadline, during startup) did not see the init, so it is repeated now.
	// Bubblewrap's death alone may not end the init: it sets its parent-death
	// signal late in its own startup.
	p.mu.Lock()
	p.initFD = fd
	expired := p.killed
	if expired {
		p.signal()
	}
	p.mu.Unlock()
	if fd < 0 || expired {
		if fd < 0 {
			// No verified init: Wait could not prove the namespace empty.
			// Bubblewrap's process group holds the init. The one numeric
			// signal, sent before the waiter starts: bubblewrap, the
			// group's leader, is not reaped, so the group id is its own.
			p.Kill()
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		go waiter()
		_ = p.Wait()
		p.stdout.Close()
		// Never the stderr tail: it may hold secrets (Stderr).
		if fd >= 0 {
			return nil, errors.New("adapterhost: the deadline passed while the adapter was starting")
		}
		return nil, errors.New("adapterhost: the adapter could not start in its PID namespace (bubblewrap)")
	}
	go waiter()
	return p, nil
}

// startupHook, when a test sets it, runs between the init's acquisition and
// its publication.
var startupHook func(p *Process, initFD int)

// verified reports whether the init's exit was seen: never without its pidfd.
func verified(initFD int) bool { return initFD >= 0 && awaitExit(initFD) }

// poll is unix.Poll; a test replaces it to make polling fail.
var poll = unix.Poll

// ErrSpawn marks an adapter that could not be started at all (its process,
// or a container's client, never ran): the host's fault, never the
// candidate's (Opus 22a-5d-4 M1).
var ErrSpawn = errors.New("adapterhost: the adapter could not be spawned")

// ErrUnverified is Wait's error when the namespace's init was not seen to
// exit (its pidfd missing or its poll failed), or a container's removal
// failed (Container): the process tree may still run, and nothing it wrote
// may be ingested.
var ErrUnverified = errors.New("adapterhost: the adapter's process tree was not seen to end")

// openInit is openChild; a test replaces it to simulate a failed acquisition.
var openInit = openChild

// openChild opens a pidfd on pid if it is parent's child. Parent is
// bubblewrap, not yet reaped (the waiter has not started), so its pid is
// its own; a pid reused after the init was reaped has another parent. -1
// when it cannot be opened or is not that child: Kill then reaches the
// namespace through bubblewrap (--die-with-parent).
func openChild(pid, parent int) int {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return -1
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	i := bytes.LastIndexByte(stat, ')')
	if err != nil || i < 0 {
		unix.Close(fd)
		return -1
	}
	if f := strings.Fields(string(stat[i+1:])); len(f) < 2 || f[1] != strconv.Itoa(parent) {
		unix.Close(fd)
		return -1
	}
	return fd
}

// awaitExit blocks until the pidfd's process has exited (for a PID
// namespace's init, once the namespace is empty) and reports whether it saw
// that; a failed poll is false, never an exit.
func awaitExit(fd int) bool {
	for {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, err := poll(fds, -1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err == nil && fds[0].Revents&unix.POLLIN != 0
	}
}

func closeAll(fs ...*os.File) {
	for _, f := range fs {
		f.Close()
	}
}

// Write keeps the stderr tail (io.Writer for the drain).
func (p *Process) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stderr = append(p.stderr, b...)
	if over := len(p.stderr) - StderrTail; over > 0 {
		p.stderr = append([]byte(nil), p.stderr[over:]...)
	}
	return len(b), nil
}

// Stderr is the last StderrTail bytes the adapter wrote to stderr, as
// written. It may contain secrets the adapter was given (a gateway token, a
// native login): this package never puts it in an error or a log, and a
// caller must redact it before storing or showing it.
func (p *Process) Stderr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return string(p.stderr)
}

// Stdin is the adapter's input. Stdout is its output, which its reader
// closes: it ends once the process tree is gone.
func (p *Process) Stdin() io.WriteCloser { return p.stdin }
func (p *Process) Stdout() io.ReadCloser { return p.stdout }

// Expired reports whether the deadline killed the process.
func (p *Process) Expired() bool { return p.expired.Load() }

// Kill ends the namespace: SIGKILL to its init (everything in the namespace
// dies with it) and to bubblewrap (whose death kills the init too,
// --die-with-parent), each through its pidfd. Idempotent; after Wait it
// sends nothing.
func (p *Process) Kill() { p.kill() }

// kill returns how many signals it sent.
func (p *Process) kill() int {
	p.mu.Lock()
	defer p.mu.Unlock() // the waiter closes the pidfds under mu
	p.killed = true
	if p.stop != nil {
		p.stop()
	}
	return p.signal()
}

// signal sends SIGKILL through each open pidfd; mu is held.
func (p *Process) signal() int {
	sent := 0
	for _, t := range []struct {
		fd   int
		init bool
	}{{p.initFD, true}, {p.bwrapFD, false}} {
		if t.fd >= 0 && sendKill(t.fd, t.init) == nil {
			sent++
		}
	}
	return sent
}

// sendKill sends SIGKILL through a pidfd, init telling whether it is the
// namespace's init's. A test replaces it to see which process was asked.
var sendKill = func(fd int, init bool) error { return unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0) }

// Done is closed once Wait would return.
func (p *Process) Done() <-chan struct{} { return p.exited }

// Wait returns after bubblewrap and the namespace's init have exited and the
// adapter's stderr has ended: the process tree is gone. It returns
// bubblewrap's exit error, or ErrUnverified when the init's exit was not
// seen (then the tree may not be gone).
func (p *Process) Wait() error {
	<-p.exited
	p.timer.Stop()
	p.stdin.Close()
	return p.waitErr
}
