// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"path/filepath"
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
// Command, under bubblewrap, with Env its whole environment, and the
// gateway's socket (ADC_GATEWAY_SOCKET), jailed (Opus 22a-5c-2a M1): it
// reads only the system's /usr, /lib*, /bin and /sbin and the adapter's own
// directory, which must hold the adapter's interpreter if /usr does not;
// it writes only the attempt's workspace and exchange and its socket; it
// has a /tmp of its own and no network. Nothing of the controller's state
// directory (the event store, the blobs, the secrets, the clones) is in
// its namespace.
type hostAdapter struct {
	Command []string
	Env     []string
}

var _ AdapterStarter = hostAdapter{}

// systemDirs are what a jailed adapter reads of the host's system.
var systemDirs = []string{"/usr", "/lib", "/lib64", "/bin", "/sbin"}

// adapterEnv is a host adapter's environment besides its socket: a fixed
// PATH, never the controller's own environment (its credentials).
var adapterEnv = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}

func (h hostAdapter) Start(run AdapterRun) (AdapterSession, error) {
	if len(h.Command) == 0 {
		return AdapterSession{}, errors.New("daemon: no adapter is configured (adcd run --adapter)")
	}
	if run.Deadline <= 0 {
		return AdapterSession{}, errors.New("daemon: an adapter session needs a positive deadline")
	}
	env := append(slices.Clone(h.Env), "ADC_GATEWAY_SOCKET="+run.Socket) // the http-chat adapter's way to the gateway
	jail := &session.Jail{ReadOnly: append(slices.Clone(systemDirs), filepath.Dir(h.Command[0])), Writable: []string{run.Workspace, run.Exchange, run.Socket}}
	return AdapterSession{Config: session.Config{Command: slices.Clone(h.Command), Env: env, Deadline: run.Deadline, Jail: jail},
		Path: func(p string) string { return p }}, nil
}
