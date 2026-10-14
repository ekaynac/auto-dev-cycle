// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
)

// maxStderr bounds a git's diagnostics (a var: tests lower it). A hostile
// tree can make git write many path-bearing warnings with little output.
var maxStderr int64 = 1 << 20

// bounded is one output stream of a command, kept up to max bytes. The
// write that would pass max kills the command instead, so nothing waits on
// a pipe nobody reads, and marks the overflow.
type bounded struct {
	buf  bytes.Buffer
	max  int64
	over *atomic.Bool
	kill func()
}

func (w *bounded) Write(p []byte) (int, error) {
	if int64(w.buf.Len())+int64(len(p)) > w.max {
		w.over.Store(true)
		w.kill()
		return 0, ErrTooLarge
	}
	return w.buf.Write(p)
}

// drain runs cmd to its end, its stdout bounded by maxOut and its stderr
// by maxErr, both drained concurrently (os/exec copies each stream on its
// own goroutine). On either overflow the command's whole process group is
// killed and reaped, and drain returns ErrTooLarge: never more than the
// bounds are held, and no descendant holding a pipe delays the end past
// WaitDelay.
func drain(cmd *exec.Cmd, maxOut, maxErr int64) (out, stderr []byte, err error) {
	var over atomic.Bool
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.WaitDelay = time.Second
	kill := func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	o := &bounded{max: maxOut, over: &over, kill: kill}
	e := &bounded{max: maxErr, over: &over, kill: kill}
	cmd.Stdout, cmd.Stderr = o, e
	err = cmd.Run() // waits for the process and both copies: reaped
	if over.Load() {
		return nil, nil, ErrTooLarge
	}
	return o.buf.Bytes(), e.buf.Bytes(), err
}
