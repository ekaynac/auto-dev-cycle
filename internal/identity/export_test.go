// SPDX-License-Identifier: Apache-2.0

package identity

import "sync/atomic"

// CountHashes counts argon2id computations until restore is called.
func CountHashes() (count func() int64, restore func()) {
	var n atomic.Int64
	orig := idKey
	idKey = func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
		n.Add(1)
		return orig(password, salt, time, memory, threads, keyLen)
	}
	return n.Load, func() { idKey = orig }
}

// Hash mints a hash of secret with any cost, as a foreign record might
// carry it.
var Hash = func(secret string, passes, memory uint32, threads uint8) string {
	return phc(secret, make([]byte, saltBytes), passes, memory, threads, keyBytes)
}
