// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// Review: a reviewer's run, one fact for both of its differences (Mythos
	// #407 L1): no base is checked out (its workspace is empty), so the
	// image's lockfile set is checked, never hashed at the workspace (chunk
	// 4a H1); and the workspace is mounted read-only (chunk 4a L1).
	Review bool
}

// AdapterSession is a started adapter's session config; Path, a host path
// under run's Workspace, Exchange or Socket as the adapter sees it; Ready,
// which the run calls once the exchange is prepared and before the launch
// (nil: nothing to do), and whose failure starts nothing; and Within, which
// the run calls once the session has ended and its process was seen gone
// (nil: nothing to check): sandbox.ErrLeft without sandbox.ErrUnmeasured
// is a run that left more than its disk bound, or what it made
// unmeasurable (Opus #393 M2); any other error keeps the attempt (Opus
// 22a-5d-4 L1, Mythos chunk 2 L3).
type AdapterSession struct {
	Config session.Config
	Path   func(string) string
	Ready  func() error
	Within func() error
}

// The adapter in a prepared image (scripts/prepare-go-image.sh): the
// release's lib/adc closure under imageLib, run by its pinned Node.
const imageLib = "/opt/adc/lib"

// resolveTimeout bounds Start's resolution of the pinned image (podman
// image inspect): a client stuck on its storage's lock is the host's fault.
var resolveTimeout = 30 * time.Second

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
	// Want is the installed release's adapter digest (sandbox.AdapterDigest
	// of its lib/adc, releaseAdapter): what an image's adapter must have.
	Want string
}

var _ AdapterStarter = (*containerAdapter)(nil)

// containerBox is what containerAdapter needs of the sandbox.
type containerBox interface {
	Resolve(ctx context.Context, ref string) (sandbox.Image, error)
	Interactive(r sandbox.Run, attempt string) (sandbox.Interactive, error)
}

func (c *containerAdapter) Start(ctx context.Context, run AdapterRun) (AdapterSession, error) {
	if run.Box == nil || run.Deadline <= 0 || run.Box.Workspace() != run.Workspace {
		return AdapterSession{}, errors.New("daemon: an adapter container needs its attempt's directory, its workspace and a positive deadline")
	}
	rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	img, err := c.Box.Resolve(rctx, run.Image)
	if rctx.Err() != nil && ctx.Err() == nil { // the host's podman hung, not the profile's fault (Mythos chunk 2 L1)
		err = fmt.Errorf("%w: the image could not be resolved within %s: %w", errHost, resolveTimeout, err)
	}
	cancel()
	switch {
	case err != nil:
	case run.Review:
		err = img.CheckPrepared(run.Toolchain, run.Lockfiles)
	default:
		err = img.CheckManifest(run.Toolchain, run.Workspace, run.Lockfiles)
	}
	if err == nil {
		err = img.CheckAdapter(c.Want)
	}
	if err != nil {
		return AdapterSession{}, err
	}
	limits := containerLimits
	limits.Timeout = run.Deadline
	r := sandbox.Run{Image: img, Mode: sandbox.Build, Attempt: run.Box, Gateway: run.Socket, Exchange: run.Exchange, ReadOnlyWorkspace: run.Review,
		Limits: limits, Argv: containerArgv}
	// The container's command is made once the exchange is (Ready): the
	// sandbox refuses an exchange whose in/ and out/ are not directories.
	ct := &session.Container{}
	var within func() error
	ready := func() error {
		it, err := c.Box.Interactive(r, string(run.Attempt))
		if err != nil {
			return fmt.Errorf("%w: %w", errHost, err)
		}
		ct.Command, ct.Env, ct.Remove, within = it.Command, it.Env, it.Remove, it.Within
		return nil
	}
	paths := map[string]string{run.Workspace: sandbox.WorkspaceDir, filepath.Join(run.Exchange, "in"): sandbox.ExchangeIn,
		filepath.Join(run.Exchange, "out"): sandbox.ExchangeOut, run.Socket: sandbox.GatewaySocket}
	return AdapterSession{Config: session.Config{Container: ct, Deadline: run.Deadline}, Ready: ready,
		Within: func() error {
			if within == nil {
				return errors.New("daemon: the adapter's container was never made")
			}
			return within()
		},
		// Any other path has no view in the container: "", which prepare refuses.
		Path: func(p string) string { return paths[p] }}, nil
}

// executable is os.Executable; a row plants a release.
var executable = os.Executable

// releaseLib is adcd's own installed release's lib/adc: <root>/bin/adcd,
// resolved, gives <root>. It holds the adapter closure (releaseAdapter)
// and the reviewer qualification's suite and records (reviewsAtStart); no
// run mounts it (controllerPaths).
func releaseLib() (string, error) {
	exe, err := executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(filepath.Dir(exe)), "lib", "adc"), nil
}

// releaseAdapter is the digest of adcd's own installed release's adapter
// closure: <root>/bin/adcd, resolved, gives <root>, whose lib/adc it digests
// (sandbox.AdapterDigest). adcd computes it once, at start (runnerOf), and
// does not start without it (Opus #393 L1): a build is never placed on a
// runner that would refuse every image.
func releaseAdapter() (string, error) {
	lib, err := releaseLib()
	if err != nil {
		return "", err
	}
	d, err := sandbox.AdapterDigest(lib)
	if err != nil {
		return "", fmt.Errorf("%w: the installed release's adapter (%s) cannot be digested: %w", ErrConfig, lib, err)
	}
	return d, nil
}
