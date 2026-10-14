// SPDX-License-Identifier: Apache-2.0

//go:build linux

package accept

import (
	"bytes"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// maxStderr bounds a git's diagnostics (a var: tests lower it). A hostile
// tree can make git write many path-bearing warnings with little output.
var maxStderr int64 = 1 << 20

// pipeDelay bounds how long drain reads after the command exits: a
// descendant still holding a pipe is cut off then (a var: tests lower it).
var pipeDelay = time.Second

// drain runs cmd in its own process group, its stdout bounded by maxOut
// and its stderr by maxErr, both drained concurrently. On either overflow
// the group is killed once, and drain returns ErrTooLarge: never more than
// the bounds are held.
//
// Ordering, so no signal reaches a reused group: the leader is reaped
// (Wait) only after both streams are drained and the leader is waitable
// (waitid WNOWAIT), and every group kill happens in a drain, before Wait.
// An unreaped leader keeps its pid, so the group id cannot be reused.
// After the leader exits, a descendant still holding a pipe delays the end
// by pipeDelay at most. A descendant that leaves the group (setsid,
// setpgid) is not contained: the group kill does not reach it.
func drain(cmd *exec.Cmd, maxOut, maxErr int64) (out, stderr []byte, err error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	or, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	er, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	var (
		over       atomic.Bool
		once       sync.Once
		wg         sync.WaitGroup
		bufs       [2]bytes.Buffer
		readersEnd = make(chan struct{})
		leaderEnd  = make(chan struct{})
	)
	pgid := cmd.Process.Pid
	kill := func() { once.Do(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }) }
	for i, s := range []struct {
		r   io.Reader
		max int64
	}{{or, maxOut}, {er, maxErr}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, _ := io.Copy(&bufs[i], io.LimitReader(s.r, s.max+1))
			if n > s.max {
				over.Store(true)
				kill()
			}
		}()
	}
	go func() { wg.Wait(); close(readersEnd) }()
	go func() {
		defer close(leaderEnd)
		waitable(pgid)
		select {
		case <-readersEnd:
		case <-time.After(pipeDelay): // a descendant holds a pipe: cut it off
			_, _ = or.Close(), er.Close()
		}
	}()
	<-readersEnd
	<-leaderEnd
	err = cmd.Wait() // reaps the leader: no kill follows
	if over.Load() {
		return nil, nil, ErrTooLarge
	}
	return bufs[0].Bytes(), bufs[1].Bytes(), err
}

// waitable blocks until the child pid has exited, without reaping it.
func waitable(pid int) {
	var info [128]byte // a siginfo_t
	for {
		_, _, e := syscall.Syscall6(syscall.SYS_WAITID, 1 /* P_PID */, uintptr(pid),
			uintptr(unsafe.Pointer(&info[0])), syscall.WEXITED|syscall.WNOWAIT, 0, 0)
		if e != syscall.EINTR {
			return
		}
	}
}
