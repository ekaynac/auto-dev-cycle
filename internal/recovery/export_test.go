// SPDX-License-Identifier: Apache-2.0

//go:build linux

package recovery

// The orphan stage's variables, for its tests: the roots recorded a start, the largest size recorded, the rename, and
// the refusal command.
var (
	MaxRoots = &maxRoots
	MaxSize  = &maxSize
	Renameat = &renameat
	Submit   = &submitRefusal
)
