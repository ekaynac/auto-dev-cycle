// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package client

import (
	"errors"
	"net"
)

// peerUID: the controller runs on Linux; elsewhere no peer is verified, so
// nothing is sent.
func peerUID(*net.UnixConn) (int, error) {
	return 0, errors.New("peer credentials are read only on Linux")
}
