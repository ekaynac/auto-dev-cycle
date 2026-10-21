// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// removeTimeout bounds one removal: the container's, and the check that it
// is gone.
const removeTimeout = 30 * time.Second

// containerPrefix starts every run's container name: an interactive run's
// is adc-<attempt>, a batch run's (Start) adc-<24 hex>.
const containerPrefix = "adc-"

// ContainerName is attempt's interactive container's name: a restart finds
// it by its attempt (RemoveStale).
func ContainerName(attempt string) string { return containerPrefix + attempt }

// ExchangeRoot is where an interactive run's exchange roots lie (P22a-5d):
// on the runs filesystem, apart from every attempt's directory (runs/a*).
// The caller makes it, as adapterhost's root.
func (h Host) ExchangeRoot() string { return filepath.Join(h.Root, "runs", "exchange") }

// Interactive is an adapter's container run (P22a-5d), which the caller
// starts and talks to over its stdin and stdout (adapterhost's session):
// Command is the podman client of `podman run --interactive`, with Start's
// confinement, Run.Exchange mounted and the gateway at GatewaySocket.
type Interactive struct {
	Command []string // argv[0] is Host.Podman
	Env     []string // Podman's whole environment: nothing of the controller's
	// Deadline is the run's Limits.Timeout: the session's deadline.
	Deadline time.Duration
	// Remove removes the container this run created, by the id Podman
	// wrote to the attempt's cid file (never by name: another run may hold
	// it; #374 M2), then checks that Podman lists it no more (with no cid
	// file, nothing was created: it removes nothing and checks the name),
	// then that what the run left, its exchange's out/ included, is within
	// Limits.Disk. Each call does all of it afresh. The attempt is unsafe
	// (CopyTree and CopyOut refuse it) from Interactive until a call has
	// seen the container gone: the podman client's exit does not mean the
	// container's has (conmon runs it).
	Remove func(ctx context.Context) error
}

// cidFile is where an interactive run's container id is written: the
// attempt's directory, outside every mount.
func cidFile(attemptDir string) string { return filepath.Join(attemptDir, "cid") }

// Interactive checks r, an interactive build run of attempt, and returns
// its command; nothing runs. The attempt is unsafe from now until Remove
// succeeds.
func (h Host) Interactive(r Run, attempt string) (Interactive, error) {
	if r.Exchange == "" || ids.Check(attempt, "att") != nil || filepath.Base(r.Exchange) != "exchange-"+attempt {
		return Interactive{}, fmt.Errorf("%w: an interactive run needs its attempt's exchange", ErrSpec)
	}
	name := ContainerName(attempt)
	args, err := h.args(r, name)
	if err != nil {
		return Interactive{}, err
	}
	global, err := h.global()
	if err != nil {
		return Interactive{}, err
	}
	a, disk, out := r.Attempt, r.Limits.Disk, filepath.Join(resolve(r.Exchange), "out")
	a.unsafe = true
	remove := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
		defer cancel()
		err := h.removeRun(ctx, cidFile(a.Dir), name)
		a.unsafe = err != nil
		if err != nil {
			return err
		}
		return a.within(disk, out)
	}
	return Interactive{Command: append(append([]string{h.Podman}, global...), args...), Env: h.env(), Deadline: r.Limits.Timeout, Remove: remove}, nil
}

var containerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// removeRun removes the container whose id is in cid, if Podman wrote one,
// then checks that Podman lists it (by id, else name) no more.
func (h Host) removeRun(ctx context.Context, cid, name string) error {
	b, err := os.ReadFile(cid)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return h.gone(ctx, "name=^"+name+"$")
	case err != nil:
		return err
	}
	id := strings.TrimSpace(string(b))
	if !containerID.MatchString(id) {
		return errors.New("sandbox: the run's cid file holds no container id")
	}
	if _, err := h.podman(ctx, "rm", "--force", "--ignore", "--time=0", id); err != nil {
		return err
	}
	return h.gone(ctx, "id="+id)
}

// gone fails unless Podman lists no container matching filter.
func (h Host) gone(ctx context.Context, filter string) error {
	left, err := h.listed(ctx, filter)
	if err == nil && len(left) > 0 {
		err = errors.New("sandbox: the container is still listed after its removal")
	}
	return err
}

// listed are the names of the containers, running or not, matching filter
// (a podman ps filter: name=<regular expression>, or id=<id>).
func (h Host) listed(ctx context.Context, filter string) ([]string, error) {
	out, err := h.podman(ctx, "ps", "--all", "--filter="+filter, "--format={{.Names}}")
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// batchName is a batch run's container name (Start).
var batchName = regexp.MustCompile(`^adc-[0-9a-f]{24}$`)

// RemoveStale is the start sweep, run before any run starts: it removes
// every batch run's container (none survives a restart) and every
// interactive container whose attempt keep does not hold (a crashed
// controller leaves both running, under conmon), and returns the attempts
// it removed. It fails unless each is then listed no more.
func (h Host) RemoveStale(ctx context.Context, keep func(attempt string) bool) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
	defer cancel()
	var removed []string
	for _, filter := range []string{"name=^" + containerPrefix + "att_", "name=^" + containerPrefix + "[0-9a-f]{24}$"} {
		names, err := h.listed(ctx, filter)
		if err != nil {
			return removed, err
		}
		for _, n := range names {
			attempt := strings.TrimPrefix(n, containerPrefix)
			interactive := ids.Check(attempt, "att") == nil
			if !interactive && !batchName.MatchString(n) || interactive && keep(attempt) {
				continue
			}
			if _, err := h.podman(ctx, "rm", "--force", "--ignore", "--time=0", n); err != nil {
				return removed, err
			}
			if err := h.gone(ctx, "name=^"+n+"$"); err != nil {
				return removed, err
			}
			if interactive {
				removed = append(removed, attempt)
			}
		}
	}
	return removed, nil
}
