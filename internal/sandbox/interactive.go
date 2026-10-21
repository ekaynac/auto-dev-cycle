// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// removeTimeout bounds one removal: the container's, and the check that it
// is gone.
const removeTimeout = 30 * time.Second

// containerPrefix starts an interactive run's container name: adc-<attempt>.
// Start's batch runs are adc-<24 hex>, never an attempt id.
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
	// Remove removes the container and returns nil only once Podman lists
	// it no more and what the run left is within Limits.Disk. Until it has
	// returned nil the attempt is unsafe (CopyTree and CopyOut refuse it):
	// the podman client's exit does not mean the container's has (conmon
	// runs it). Every call returns the first call's result.
	Remove func(ctx context.Context) error
}

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
	a, disk := r.Attempt, r.Limits.Disk
	a.unsafe = true
	var once sync.Once
	var rmErr error
	remove := func(ctx context.Context) error {
		once.Do(func() {
			if rmErr = h.remove(ctx, name); rmErr == nil {
				a.unsafe = false
				rmErr = a.within(disk)
			}
		})
		return rmErr
	}
	return Interactive{Command: append(append([]string{h.Podman}, global...), args...), Env: h.env(), Deadline: r.Limits.Timeout, Remove: remove}, nil
}

// remove removes the container name, then checks that Podman lists it no
// more, under a bound of the controller's own.
func (h Host) remove(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
	defer cancel()
	if _, err := h.podman(ctx, "rm", "--force", "--ignore", "--time=0", name); err != nil {
		return err
	}
	return h.gone(ctx, name)
}

// gone fails unless Podman lists no container named name.
func (h Host) gone(ctx context.Context, name string) error {
	left, err := h.listed(ctx, "^"+name+"$")
	if err == nil && len(left) > 0 {
		err = errors.New("sandbox: the container is still listed after its removal")
	}
	return err
}

// listed are the names of the containers, running or not, matching the
// name filter (a regular expression).
func (h Host) listed(ctx context.Context, filter string) ([]string, error) {
	out, err := h.podman(ctx, "ps", "--all", "--filter=name="+filter, "--format={{.Names}}")
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// RemoveStale removes every interactive container whose attempt keep does
// not hold (P22a-5d: a crashed controller leaves its adapters' containers
// running, under conmon) and returns the attempts it removed. It fails
// unless each is then listed no more.
func (h Host) RemoveStale(ctx context.Context, keep func(attempt string) bool) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
	defer cancel()
	names, err := h.listed(ctx, "^"+containerPrefix+"att_")
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, n := range names {
		attempt := strings.TrimPrefix(n, containerPrefix)
		if ids.Check(attempt, "att") != nil || keep(attempt) {
			continue
		}
		if _, err := h.podman(ctx, "rm", "--force", "--ignore", "--time=0", n); err != nil {
			return removed, err
		}
		if err := h.gone(ctx, n); err != nil {
			return removed, err
		}
		removed = append(removed, attempt)
	}
	return removed, nil
}
