// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

func init() {
	// ppid says its parent's pid, then echoes.
	stubs["ppid"] = func() {
		fmt.Println(os.Getppid())
		_, _ = io.Copy(os.Stdout, os.Stdin)
	}
	// linger ignores its input's end and lingers until killed.
	stubs["linger"] = func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		time.Sleep(time.Hour)
	}
	// monitor plays a container runtime's client: it leaves a process (the
	// monitor) holding its stderr, says that process's pid and exits.
	stubs["monitor"] = func() {
		c := exec.Command("sleep", os.Getenv("STUB_MARKER"))
		c.Stderr = os.Stderr
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if c.Start() != nil {
			os.Exit(3)
		}
		fmt.Println(c.Process.Pid)
	}
}

// removal is a Container's Remove that counts its calls and returns err.
type removal struct {
	calls atomic.Int32
	err   error
	do    func()
}

func (r *removal) remove(context.Context) error {
	r.calls.Add(1)
	if r.do != nil {
		r.do()
	}
	return r.err
}

func container(t *testing.T, mode string, rm *removal, env ...string) *Container {
	t.Helper()
	spec := stubSpec(t, mode, env...)
	return &Container{Command: spec.Command, Env: spec.Env, Remove: rm.remove}
}

func startC(t *testing.T, c *Container, deadline time.Duration) *Process {
	t.Helper()
	p, err := StartContainer(c, deadline)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Kill()
		_ = p.Wait()
		p.Stdout().Close()
	})
	return p
}

func TestContainerNeedsACommandARemovalAndADeadline(t *testing.T) {
	rm := func(context.Context) error { return nil }
	for _, c := range []*Container{nil, {Remove: rm}, {Command: []string{"true"}}} {
		if _, err := StartContainer(c, time.Minute); err == nil {
			t.Errorf("StartContainer(%+v) succeeded", c)
		}
	}
	if _, err := StartContainer(&Container{Command: []string{"true"}, Remove: rm}, 0); err == nil {
		t.Error("a container with no deadline started")
	}
}

// P22a-5d: the client runs as this process's own child, never under
// bubblewrap (none is on PATH here: Spawn could not start); its stdio is
// the session's; its end is removed once, after it exits, and Wait returns.
func TestContainerIsNotUnderBubblewrap(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := Spawn(stubSpec(t, "echo")); err == nil {
		t.Fatal("Spawn found bubblewrap, so this proves nothing")
	}
	rm := &removal{}
	p := startC(t, container(t, "ppid", rm), time.Minute)
	out := bufio.NewReader(p.Stdout())
	line, err := out.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("the client's parent: %q, %v; want %d", line, err, os.Getpid())
	}
	if _, err := p.Stdin().Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := out.ReadString('\n'); err != nil || line != "hello\n" {
		t.Fatalf("read %q, %v", line, err)
	}
	p.Stdin().Close() // EOF reaches the client
	if err := p.Wait(); err != nil || p.Expired() || rm.calls.Load() != 1 {
		t.Fatalf("exit %v, expired %v, %d removals", err, p.Expired(), rm.calls.Load())
	}
}

// Any doubt that the container is gone (a removal that fails) is Wait's
// ErrUnverified, whatever the client's exit.
func TestContainerAFailedRemovalIsUnverified(t *testing.T) {
	rm := &removal{err: errors.New("still listed")}
	p := startC(t, container(t, "echo", rm), time.Minute)
	p.Stdin().Close()
	if err := p.Wait(); !errors.Is(err, ErrUnverified) || rm.calls.Load() != 1 {
		t.Fatalf("Wait %v (%d removals), want ErrUnverified", err, rm.calls.Load())
	}
}

// Kill, however often, and the deadline start one early removal, and Wait
// runs one fresh removal after the client is killed: two in all.
func TestContainerKillAndTheDeadlineRemove(t *testing.T) {
	rm := &removal{}
	p := startC(t, container(t, "linger", rm), time.Hour)
	p.Kill()
	p.Kill()
	if err := p.Wait(); err == nil || errors.Is(err, ErrUnverified) || p.Expired() || rm.calls.Load() != 2 {
		t.Fatalf("killed: Wait %v, expired %v, %d removals", err, p.Expired(), rm.calls.Load())
	}
	rm = &removal{}
	p = startC(t, container(t, "linger", rm), time.Millisecond)
	if err := p.Wait(); err == nil || !p.Expired() || rm.calls.Load() != 2 {
		t.Fatalf("deadline: Wait %v, expired %v, %d removals", err, p.Expired(), rm.calls.Load())
	}
}

// The runtime's monitor holds stderr after the client exits: the removal
// runs first and ends it, so Wait returns, with the monitor gone.
func TestContainerRemovalPrecedesTheDrain(t *testing.T) {
	marker := newMarker()
	pid := make(chan int, 1)
	rm := &removal{do: func() { _ = syscall.Kill(<-pid, syscall.SIGKILL) }}
	p := startC(t, container(t, "monitor", rm, "STUB_MARKER="+marker), time.Hour)
	line, err := bufio.NewReader(p.Stdout()).ReadString('\n')
	n, convErr := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || convErr != nil {
		t.Fatalf("the monitor's pid: %q, %v", line, err)
	}
	pid <- n
	if err := p.Wait(); err != nil || rm.calls.Load() != 1 {
		t.Fatalf("Wait %v, %d removals", err, rm.calls.Load())
	}
	if running(marker) {
		t.Fatal("the monitor outlived Wait")
	}
}

// Open runs a Container through the session, not Command: a clean shutdown
// removes it; a failed removal is ErrUnverified.
func TestOpenRunsAContainer(t *testing.T) {
	for _, fail := range []bool{false, true} {
		rm := &removal{}
		if fail {
			rm.err = errors.New("still listed")
		}
		cfg := scripted(t, script{"shutdown": {{Lines: []string{resultLine(cleanOutcome)}, Exit: true}}})
		s := open(t, Config{Command: []string{"/nonexistent"}, Container: &Container{Command: cfg.Command, Env: cfg.Env, Remove: rm.remove}, Deadline: cfg.Deadline})
		if _, err := plain(s, context.Background(), "shutdown"); err != nil {
			t.Fatal(err)
		}
		err := s.Wait()
		if fail != errors.Is(err, ErrUnverified) || !fail && err != nil || rm.calls.Load() < 1 {
			t.Errorf("removal failing %v: Wait %v, %d removals", fail, err, rm.calls.Load())
		}
		wantEnd(t, s, s.Err(), KindClosed, vocab.ReasonAttemptStopped)
	}
}

// With no signal delivered (the client's pidfd unusable), Kill still ends
// the run: the removal, which ends the container and so its client.
func TestContainerKillWithoutASignalRemoves(t *testing.T) {
	old := sendKill
	sendKill = func(int, bool) error { return errors.New("no pidfd") }
	t.Cleanup(func() { sendKill = old })
	rm := &removal{}
	p := startC(t, container(t, "linger", rm), time.Hour)
	rm.do = func() { _ = p.cmd.Process.Kill() } // the runtime ends the client with its container
	p.Kill()
	if err := p.Wait(); err == nil || rm.calls.Load() != 2 {
		t.Fatalf("Wait %v, %d removals", err, rm.calls.Load())
	}
}

// Wait never reuses the Kill-time removal's result: it may have run before
// the container existed. Here it said gone, the fresh one does not: Wait is
// ErrUnverified (#374 L1).
func TestContainerWaitRemovesAfresh(t *testing.T) {
	rm := &removal{}
	rm.do = func() {
		if rm.calls.Load() > 1 {
			rm.err = errors.New("still listed")
		}
	}
	p := startC(t, container(t, "linger", rm), time.Hour)
	p.Kill()
	if err := p.Wait(); !errors.Is(err, ErrUnverified) || rm.calls.Load() != 2 {
		t.Fatalf("Wait %v, %d removals; want ErrUnverified after a fresh one", err, rm.calls.Load())
	}
}
