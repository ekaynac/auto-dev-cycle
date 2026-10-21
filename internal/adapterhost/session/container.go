// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Container is an adapter in a container (P22a-5d; sandbox.Interactive):
// Command is the container runtime's client, which runs it attached (its
// stdin and stdout are the adapter's), with exactly Env. The container, not
// a PID namespace, is the boundary, so the client is never put under
// bubblewrap. Remove removes the container and returns nil only once it has
// seen it gone: the client's exit does not mean the container's has (the
// runtime's monitor, not the client, is its parent).
type Container struct {
	Command []string
	Env     []string
	Remove  func(ctx context.Context) error
}

// StartContainer is Spawn's counterpart for a Container (Open calls it for
// Config.Container): it starts c's client as the session's process. Kill (the
// deadline among its causes) kills the client through its pidfd and starts
// the removal; Wait returns after the client has exited, the removal has
// returned and stderr has ended. A removal that fails, for any reason,
// makes Wait's error ErrUnverified: the container may still run, and
// nothing it wrote may be ingested.
func StartContainer(c *Container, deadline time.Duration) (*Process, error) {
	if c == nil || len(c.Command) == 0 || c.Remove == nil || deadline <= 0 {
		return nil, errors.New("adapterhost: a container needs a command, a removal and a positive deadline")
	}
	var pipes [6]*os.File // stdin, stdout, stderr: read end, write end
	for i := 0; i < 6; i += 2 {
		var err error
		if pipes[i], pipes[i+1], err = os.Pipe(); err != nil {
			closeAll(pipes[:i]...)
			return nil, fmt.Errorf("adapterhost: pipe: %w", err)
		}
	}
	cmd := exec.Command(c.Command[0], c.Command[1:]...)
	cmd.Env, cmd.Dir = append([]string{}, c.Env...), "/"
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pipes[0], pipes[3], pipes[5]
	clientFD := -1
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, PidFD: &clientFD}
	err := cmd.Start()
	closeAll(pipes[0], pipes[3], pipes[5]) // the child's ends
	if err != nil {
		closeAll(pipes[1], pipes[2], pipes[4])
		// exec's error names the client's path, never the adapter's argv or env.
		return nil, fmt.Errorf("adapterhost: spawn: %w", err)
	}
	// The removal starts once, from Kill or from the waiter, and never
	// under the caller's context: it must run whatever the session does.
	removed := make(chan struct{})
	var once sync.Once
	var rmErr error
	remove := func() {
		once.Do(func() {
			go func() {
				defer close(removed)
				rmErr = c.Remove(context.Background())
			}()
		})
	}
	// The client's pidfd stands where bubblewrap's would (bwrapFD): Kill
	// signals it; there is no init to watch.
	p := &Process{cmd: cmd, stdin: pipes[1], stdout: pipes[2], exited: make(chan struct{}), bwrapFD: clientFD, initFD: -1, stop: remove}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(p, pipes[4]) // ends when the last holder of stderr is gone
		pipes[4].Close()
	}()
	p.timer = time.AfterFunc(deadline, func() {
		p.expired.Store(true)
		p.Kill()
	})
	go func() {
		p.waitErr = cmd.Wait()
		// The removal before the drain: whatever else holds stderr (the
		// runtime's monitor) ends only with the container.
		remove()
		<-removed
		if rmErr != nil {
			p.waitErr = ErrUnverified
		}
		<-drained
		p.mu.Lock()
		if p.bwrapFD >= 0 {
			unix.Close(p.bwrapFD)
			p.bwrapFD = -1
		}
		p.mu.Unlock()
		close(p.exited)
	}()
	return p, nil
}
