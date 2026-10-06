// SPDX-License-Identifier: Apache-2.0

//go:build linux

package client

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID is the uid of the process at the other end, from the kernel
// (SO_PEERCRED), not from the socket file's owner.
func peerUID(c *net.UnixConn) (int, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var serr error
	if err := rc.Control(func(fd uintptr) { cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	return int(cred.Uid), nil
}
