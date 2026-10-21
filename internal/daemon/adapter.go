// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
)

// AdapterStarter starts one attempt's adapter session: the one seam the
// attempt run (22a-5c-2) goes through to start the builder's adapter, which
// 22a-5d swaps to start it in the attempt's container (#226 entry 126).
// 5c-2's hostAdapter starts it on the host, under bubblewrap
// (session.Spawn), its tools confined to the workspace by the adapter.
type AdapterStarter interface {
	// Start is run's adapter session: the session's config, and how the
	// adapter sees the paths it is handed.
	Start(run AdapterRun) (AdapterSession, error)
}

// AdapterRun is what one attempt's adapter is started over, as the
// controller has it on the host.
type AdapterRun struct {
	Attempt   types.AttemptID
	Workspace string        // the attempt's workspace (prepare's workspace.path)
	Exchange  string        // its exchange root (adapterhost.ExchangeName under the host's root): in/ and out/
	Socket    string        // its gateway socket (egress.open's)
	Deadline  time.Duration // the whole session, from spawn
}

// AdapterSession is a started adapter's session config, and Path: a host
// path under run's Workspace, Exchange or Socket as the adapter sees it
// (the identity on the host; a container maps its mounts).
type AdapterSession struct {
	Config session.Config
	Path   func(string) string
}

// hostAdapter starts the adapter on the host (#226 entry 126, option A):
// Command, under bubblewrap, with Env its whole environment.
type hostAdapter struct {
	Command []string
	Env     []string
}

var _ AdapterStarter = hostAdapter{}

func (h hostAdapter) Start(run AdapterRun) (AdapterSession, error) {
	if len(h.Command) == 0 {
		return AdapterSession{}, errors.New("daemon: no adapter is configured (adcd run --adapter)")
	}
	if run.Deadline <= 0 {
		return AdapterSession{}, errors.New("daemon: an adapter session needs a positive deadline")
	}
	return AdapterSession{Config: session.Config{Command: slices.Clone(h.Command), Env: slices.Clone(h.Env), Deadline: run.Deadline},
		Path: func(p string) string { return p }}, nil
}
