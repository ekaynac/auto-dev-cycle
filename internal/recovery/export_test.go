// SPDX-License-Identifier: Apache-2.0

//go:build linux

package recovery

// The orphan stage's variables, for its tests: the roots refused a start, the largest size recorded, the rename.
var (
	MaxRoots = &maxRoots
	MaxSize  = &maxSize
	Renameat = &renameat
)
