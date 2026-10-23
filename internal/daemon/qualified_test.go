// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capability"
	"github.com/ekaynac/auto-dev-cycle/internal/qualify"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
)

// qualifiedAt plants the qualification's root (this repository, whose
// suite and adapter qualify.Load reads) and records (none: an empty
// directory) for the test.
func qualifiedAt(t *testing.T) string {
	t.Helper()
	records := t.TempDir()
	qualificationAt = func() (string, string, error) { return "../..", records, nil }
	t.Cleanup(func() {
		qualificationAt = func() (string, string, error) {
			lib, err := releaseLib()
			return lib, qualify.RecordsDir(lib), err
		}
	})
	return records
}

// P31 at adcd's start (P22a-6; the lead's rulings): review's qualification
// (capability.ReviewQualified) is derived at the start from the release's
// records, every registered reviewer qualified or review inactive, its
// reason one start line; the start is never refused. A record that
// qualifies is made only by qualify.Route.Review (qualify's TestReviewers
// covers records of another route or executor with genuine test runs); a
// passing check is planted here.
func TestReviewIsDerivedAtStart(t *testing.T) {
	t.Run("no record", func(t *testing.T) {
		qualifiedAt(t)
		var log logBuffer
		reg := stubRegistry(t, nil, nil)
		active, open := reviewsAtStart(reg, &log)
		reviewer := reg.Executors(types.RegistryExecutorRoleReviewer)[0]
		want := "adcd: review stays inactive: " + capability.ReviewQualified + ": reviewer " + string(reviewer) + ": qualify: the reviewer route is not qualified: no record of the route\n"
		if active || log.String() != want || len(open) != 1 || !strings.HasPrefix(open[0], capability.ReviewQualified+": reviewer ") {
			t.Errorf("no record: active %v, open %v, log %q", active, open, log.String())
		}
	})
	t.Run("no registry", func(t *testing.T) { // Mythos #337 L2: stated at the start, never a refused one
		qualifiedAt(t)
		var log logBuffer
		if active, _ := reviewsAtStart(registry.Registry{}, &log); active || !strings.Contains(log.String(), "no reviewer is registered (--registry)") {
			t.Errorf("no registry: active %v, log %q", active, log.String())
		}
		if err := (Config{StateDir: "/srv/adc/state"}).Validate(); err != nil {
			t.Errorf("a configuration with no registry is refused: %v", err)
		}
	})
	t.Run("no release", func(t *testing.T) {
		executable = func() (string, error) { return "", errors.New("no executable") }
		t.Cleanup(func() { executable = os.Executable })
		var log logBuffer
		if active, _ := reviewsAtStart(stubRegistry(t, nil, nil), &log); active || !strings.Contains(log.String(), "the release's qualification cannot be found") {
			t.Errorf("no release: active %v, log %q", active, log.String())
		}
	})
	t.Run("qualified", func(t *testing.T) {
		records := qualifiedAt(t)
		var got []types.ExecutorID
		qualifiedReviewers = func(root, dir string, reviewers []types.RegistryExecutor) error {
			for _, e := range reviewers {
				got = append(got, e.ID)
			}
			if root != "../.." || dir != records {
				return errors.New("another root")
			}
			return nil
		}
		t.Cleanup(func() { qualifiedReviewers = qualify.Reviewers })
		var log logBuffer
		reg := stubRegistry(t, nil, nil)
		active, open := reviewsAtStart(reg, &log)
		if !active || len(open) != 0 || log.String() != "" || !slices.Equal(got, reg.Executors(types.RegistryExecutorRoleReviewer)) {
			t.Errorf("qualified: active %v (its rows kept), open %v, log %q, checked %v", active, open, log.String(), got)
		}
	})
	// The lead's condition: no runtime decision reads the table's static
	// Active for review. A planted review whose rows are all kept is
	// inactive in the table (its Proven prerequisite) and active once its
	// check passes: adcd's loop (Gates.Reviews, read at Run's ready) takes
	// the derived value, and the failing check's.
	for name, pass := range map[string]bool{"a runtime reader, derived active": true, "a runtime reader, derived inactive": false} {
		t.Run(name, func(t *testing.T) {
			qualifiedAt(t)
			planted := capability.Capability{Name: "review", Proven: []string{capability.ReviewQualified}}
			reviewCapability = func() capability.Capability { return planted }
			qualifiedReviewers = func(string, string, []types.RegistryExecutor) error {
				if pass {
					return nil
				}
				return qualify.ErrUnqualified
			}
			t.Cleanup(func() {
				reviewCapability = func() capability.Capability { c, _ := capability.Of("review"); return c }
				qualifiedReviewers = qualify.Reviewers
			})
			if planted.Active() {
				t.Fatal("the planted review is active in the static table")
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			var reviews bool
			go func() {
				done <- Run(ctx, Config{StateDir: stateDir(t)}, io.Discard, func(d *Daemon) { reviews = d.gates.Reviews; cancel() })
			}()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if reviews != pass {
				t.Errorf("adcd's loop reviews %v, its derived value %v", reviews, pass)
			}
		})
	}
}

// The release's lib/adc, which holds the qualification's sealed probes and
// records (the lead's ruling (b)), is a controller path no run mounts or
// sees (sandbox.Host.Forbidden: the mounts refuse it, the probe checks it
// absent).
func TestNoRunSeesTheReleasesLib(t *testing.T) {
	lib, err := releaseLib()
	if err != nil {
		t.Fatal(err)
	}
	if paths := (Config{StateDir: "/srv/adc/state"}).controllerPaths(); !slices.Contains(paths, filepath.Clean(lib)) {
		t.Errorf("the controller's paths %v lack the release's lib %s", paths, lib)
	}
}

// Opus #413 L1: the release's lib/adc is a controller path, so a sandbox
// root inside it is refused at start, and the refusal says so.
func TestASandboxRootInTheReleasesLibIsRefused(t *testing.T) {
	rel, err := os.MkdirTemp("", "rel")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(rel) })
	lib := filepath.Join(rel, "lib", "adc")
	var shm, tmp syscall.Stat_t
	if syscall.Stat("/dev/shm", &shm) != nil || syscall.Stat(rel, &tmp) != nil || shm.Dev == tmp.Dev {
		t.Skip("no second filesystem (/dev/shm) for runs/")
	}
	own, err := os.MkdirTemp("/dev/shm", "runs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(own) })
	for _, root := range []string{filepath.Join(lib, "s"), filepath.Join(rel, "s")} { // runs/ on a filesystem of its own: only lib/adc tells them apart
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(own, filepath.Join(root, "runs")); err != nil {
			t.Fatal(err)
		}
	}
	executable = func() (string, error) { return filepath.Join(rel, "bin", "adcd"), nil }
	t.Cleanup(func() { executable = os.Executable })
	if err := os.MkdirAll(filepath.Join(rel, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, "bin", "adcd"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	state := stateDir(t)
	if _, err := LoadConfig([]string{"--state-dir", state, "--sandbox-root", filepath.Join(rel, "s")}); err != nil {
		t.Fatalf("a sandbox root beside the release: %v", err)
	}
	_, err = LoadConfig([]string{"--state-dir", state, "--sandbox-root", filepath.Join(lib, "s")})
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "the installed release's lib/adc") || strings.Contains(err.Error(), rel) {
		t.Errorf("a sandbox root inside the release's lib/adc: %v", err)
	}
}
