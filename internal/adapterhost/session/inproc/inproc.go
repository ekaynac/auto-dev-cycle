// SPDX-License-Identifier: Apache-2.0

// Package inproc is for the contract-fixture harness only: an adapter that
// is a function of this process, run by a session over its pipes instead of
// a spawned process in its own PID namespace. Nothing contains it, so it
// must never reach a production path: only internal/fixtures, the session
// that runs it, and _test files may import this package, and a test in
// internal/adapterhost enforces that (TestInprocIsFixtureHarnessOnly). The
// fixtures need it because their installed check runs adc with neither
// bubblewrap nor node to spawn an adapter (scripts/installed-fixtures.sh).
package inproc

import "io"

// Peer is an in-process adapter: session.Config.InProcess runs it.
type Peer struct {
	run func(in io.Reader, out io.Writer)
}

// New wraps run, which must return once in or out fails: the session closes
// both to end it.
func New(run func(in io.Reader, out io.Writer)) *Peer { return &Peer{run: run} }

// Run serves the session: in is what the core writes, out what it reads.
func (p *Peer) Run(in io.Reader, out io.Writer) { p.run(in, out) }
