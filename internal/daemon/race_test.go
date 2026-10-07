// SPDX-License-Identifier: Apache-2.0

//go:build race

package daemon

// raceEnabled: the race detector slows the server enough to change
// buffer-timed tests.
const raceEnabled = true
