// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// AdapterStarter starts one attempt's adapter session: the one seam the
// attempt run (22a-5c-2) goes through to start the builder's adapter.
// adcd's is containerAdapter: the adapter runs in the attempt's container
// (22a-5d, #226 entries 126, 143).
type AdapterStarter interface {
	// Start is run's adapter session: the session's config, how the adapter
	// sees the paths it is handed, and what must hold before it is launched.
	Start(ctx context.Context, run AdapterRun) (AdapterSession, error)
}

// AdapterRun is what one attempt's adapter is started over, as the
// controller has it on the host.
type AdapterRun struct {
	Attempt   types.AttemptID
	Box       *sandbox.Attempt // the attempt's directory, whose workspace Workspace is
	Workspace string           // the attempt's workspace (prepare's workspace.path), the base checked out
	Exchange  string           // its exchange root (adapterhost.ExchangeName under the host's root): in/ and out/
	Socket    string           // its gateway socket (egress.open's)
	Deadline  time.Duration    // the whole session, from spawn
	Image     string           // the project's pinned toolchain image, name@sha256:hex
	Toolchain string           // its key in the profile's toolchain.images
	Lockfiles []string         // the profile's dependency_recipe.lockfiles, at the base
}

// AdapterSession is a started adapter's session config; Path, a host path
// under run's Workspace, Exchange or Socket as the adapter sees it; and
// Ready, which the run calls once the exchange is prepared and before the
// launch (nil: nothing to do), and whose failure starts nothing.
type AdapterSession struct {
	Config session.Config
	Path   func(string) string
	Ready  func() error
}

// The adapter in a prepared image (scripts/prepare-go-image.sh): the
// release's lib/adc closure under imageLib, run by its pinned Node.
const imageLib = "/opt/adc/lib"

var containerArgv = []string{imageLib + "/node/bin/node", imageLib + "/adapters/http-chat/src/main.ts"}

// containerLimits bound an adapter's container; its timeout is the
// session's deadline.
var containerLimits = sandbox.Limits{Memory: 2 << 30, CPUs: "2", PIDs: 256, Disk: maxTree}

// containerAdapter starts the builder's adapter in the attempt's container
// (sandbox.Host.Interactive, build mode): the project's pinned image,
// resolved by digest and checked, its manifest against the toolchain and
// the base's lockfiles, and its adapter against adcd's own installed
// release (sandbox.Image.CheckAdapter: the image's /opt/adc/lib is the
// release's lib/adc, byte for byte), before anything is prepared. The
// container mounts only the attempt's workspace and results, its exchange
// (in/ read-only) and its gateway socket: no network, no other host path.
// The reviewer's adapter (22a-6) runs here too, with no tools and a
// read-only workspace (#226 entry 143).
type containerAdapter struct {
	Box containerBox // the sandbox (sandbox.Host)
	// Release is the installed release's lib/adc, whose closure's digest
	// (sandbox.AdapterDigest) an image's adapter must have.
	Release string

	once   sync.Once
	digest string
	err    error
}

var _ AdapterStarter = (*containerAdapter)(nil)

// containerBox is what containerAdapter needs of the sandbox.
type containerBox interface {
	Resolve(ctx context.Context, ref string) (sandbox.Image, error)
	Interactive(r sandbox.Run, attempt string) (sandbox.Interactive, error)
}

// want is the installed release's adapter digest, computed once.
func (c *containerAdapter) want() (string, error) {
	c.once.Do(func() { c.digest, c.err = sandbox.AdapterDigest(c.Release) })
	return c.digest, c.err
}

func (c *containerAdapter) Start(ctx context.Context, run AdapterRun) (AdapterSession, error) {
	if run.Box == nil || run.Deadline <= 0 || run.Box.Workspace() != run.Workspace {
		return AdapterSession{}, errors.New("daemon: an adapter container needs its attempt's directory, its workspace and a positive deadline")
	}
	want, err := c.want()
	if err != nil {
		return AdapterSession{}, fmt.Errorf("%w: the installed release's adapter cannot be digested: %w", errConfig, err)
	}
	img, err := c.Box.Resolve(ctx, run.Image)
	if err == nil {
		err = img.CheckManifest(run.Toolchain, run.Workspace, run.Lockfiles)
	}
	if err == nil {
		err = img.CheckAdapter(want)
	}
	if err != nil {
		return AdapterSession{}, err
	}
	limits := containerLimits
	limits.Timeout = run.Deadline
	r := sandbox.Run{Image: img, Mode: sandbox.Build, Attempt: run.Box, Gateway: run.Socket, Exchange: run.Exchange, Limits: limits, Argv: containerArgv}
	// The container's command is made once the exchange is (Ready): the
	// sandbox refuses an exchange whose in/ and out/ are not directories.
	ct := &session.Container{}
	ready := func() error {
		it, err := c.Box.Interactive(r, string(run.Attempt))
		if err != nil {
			return err
		}
		ct.Command, ct.Env, ct.Remove = it.Command, it.Env, it.Remove
		return nil
	}
	paths := map[string]string{run.Workspace: sandbox.WorkspaceDir, filepath.Join(run.Exchange, "in"): sandbox.ExchangeIn,
		filepath.Join(run.Exchange, "out"): sandbox.ExchangeOut, run.Socket: sandbox.GatewaySocket}
	return AdapterSession{Config: session.Config{Container: ct, Deadline: run.Deadline}, Ready: ready,
		// Any other path has no view in the container: "", which prepare refuses.
		Path: func(p string) string { return paths[p] }}, nil
}

// releaseLib is adcd's own installed release's lib/adc: <root>/bin/adcd,
// resolved, gives <root>.
func releaseLib() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	return filepath.Join(filepath.Dir(filepath.Dir(exe)), "lib", "adc"), err
}
