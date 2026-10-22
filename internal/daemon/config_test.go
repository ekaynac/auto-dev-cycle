// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "groups-secret-name.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfig(t *testing.T) {
	state := stateDir(t)
	good, err := json.Marshal([]types.ResourceGroup{tokens(10)})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig([]string{"--state-dir", state, "--resource-groups", writeFile(t, string(good))})
	if err != nil || cfg.StateDir != state || len(cfg.Groups) != 1 || cfg.Groups[0].ID != quota {
		t.Fatalf("%+v %v", cfg, err)
	}
	if cfg, err := LoadConfig([]string{"--state-dir", state}); err != nil || cfg.Groups == nil || len(cfg.Groups) != 0 {
		t.Fatalf("no groups by default: %+v %v", cfg, err)
	}
	dup, _ := json.Marshal([]types.ResourceGroup{tokens(1), tokens(2)})
	fifo := filepath.Join(t.TempDir(), "groups.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"no state dir":          {},
		"relative state":        {"--state-dir", "state"},
		"unclean state":         {"--state-dir", state + "/../state"},
		"unknown flag":          {"--state-dir", state, "--listen", "x"},
		"extra argument":        {"--state-dir", state, "extra"},
		"missing file":          {"--state-dir", state, "--resource-groups", filepath.Join(t.TempDir(), "absent.json")},
		"not an array":          {"--state-dir", state, "--resource-groups", writeFile(t, `{"id":"x"}`)},
		"schema-invalid":        {"--state-dir", state, "--resource-groups", writeFile(t, `[{"id":"sk-not-a-group"}]`)},
		"capacity rule":         {"--state-dir", state, "--resource-groups", writeFile(t, string(dup))},
		"null instead of []":    {"--state-dir", state, "--resource-groups", writeFile(t, `null`)},
		"a FIFO":                {"--state-dir", state, "--resource-groups", fifo},
		"negative trusted head": {"--state-dir", state, "--trusted-head", "-1:sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"bad trusted head":      {"--state-dir", state, "--trusted-head", "3:md5:abc"},
	} {
		_, err := LoadConfig(args)
		if !errors.Is(err, ErrConfig) {
			t.Errorf("%s: accepted: %v", name, err)
			continue
		}
		// Fixed text: no path and no value read from the file.
		for _, leak := range []string{state, "secret-name", "sk-not-a-group", "absent"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: the error echoes %q: %v", name, leak, err)
			}
		}
	}
}

// TestLoadConfigRegistry: `adcd run --registry` checks the registry
// against the resource groups at load, and Start checks it again.
func TestLoadConfigRegistry(t *testing.T) {
	state := stateDir(t)
	const fx = "../../fixtures/m1/config/"
	groups, stub := fx+"groups.json", fx+"registry-stub.json"
	cfg, err := LoadConfig([]string{"--state-dir", state, "--resource-groups", groups, "--registry", stub})
	if err != nil || cfg.Registry == nil || len(cfg.Registry.Executors) != 2 {
		t.Fatalf("%+v %v", cfg, err)
	}
	if cfg, err := LoadConfig([]string{"--state-dir", state, "--resource-groups", groups, "--registry", fx + "registry-live.example.json"}); err != nil {
		t.Fatalf("the live example: %+v %v", cfg, err)
	}
	raw, err := os.ReadFile(stub)
	if err != nil {
		t.Fatal(err)
	}
	same := writeFile(t, strings.Replace(string(raw), `"family": "stub-reviewer"`, `"family": "stub-builder"`, 1))
	for name, c := range map[string]struct {
		args []string
		rule string
	}{
		"no groups":      {[]string{"--registry", stub}, "the group is not configured"},
		"same family":    {[]string{"--resource-groups", groups, "--registry", same}, "executor 1: a reviewer's family"},
		"missing file":   {[]string{"--resource-groups", groups, "--registry", filepath.Join(t.TempDir(), "absent.json")}, "the registry cannot be opened"},
		"not a registry": {[]string{"--resource-groups", groups, "--registry", groups}, "does not satisfy config/executor-registry"},
	} {
		_, err := LoadConfig(append([]string{"--state-dir", state}, c.args...))
		if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), c.rule) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		for _, leak := range []string{state, "secret-name", "absent", "stub-builder"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: the error echoes %q: %v", name, leak, err)
			}
		}
	}
	cfg.Registry.Executors[1].Family = cfg.Registry.Executors[0].Family // edited after load: Start checks again
	if d, err := Start(t.Context(), cfg); !errors.Is(err, ErrConfig) {
		if d != nil {
			d.Close()
		}
		t.Fatalf("Start ran a registry whose families are equal: %v", err)
	}
	var stderr bytes.Buffer
	if code := RunCommand([]string{"--state-dir", state, "--resource-groups", groups, "--registry", same}, &stderr); code != entry.ExitUsage {
		t.Fatalf("adcd run with equal families: exit %d: %s", code, stderr.String())
	}
}

func TestRunCommandUsage(t *testing.T) {
	var stderr bytes.Buffer
	if code := RunCommand([]string{"--state-dir", "relative"}, &stderr); code != entry.ExitUsage {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

func TestRunCommandAlreadyRunning(t *testing.T) {
	dir := stateDir(t)
	d, err := Start(t.Context(), Config{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var stderr bytes.Buffer
	if code := RunCommand([]string{"--state-dir", dir}, &stderr); code != entry.ExitCouldNotRun ||
		!strings.Contains(stderr.String(), "could_not_run/controller_already_running") || strings.Contains(stderr.String(), dir) {
		t.Fatalf("exit %d: %q", code, stderr.String())
	}
}

// A FIFO whose writer holds valid groups is refused unread: only a regular file
// is read, and nothing waits on a writer.
func TestReadGroupsRefusesAFIFO(t *testing.T) {
	good, err := json.Marshal([]types.ResourceGroup{tokens(10)})
	fifo := filepath.Join(t.TempDir(), "groups.json")
	if err == nil {
		err = syscall.Mkfifo(fifo, 0o600)
	}
	var w *os.File
	if err == nil {
		w, err = os.OpenFile(fifo, os.O_RDWR, 0) // a writer that never blocks
	}
	if err == nil {
		_, err = w.Write(good)
	}
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := readGroups(fifo); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrConfig) {
			t.Errorf("a FIFO: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("readGroups read from a FIFO")
	}
	w.Close() // ends a blocked read
}

// testRelease is a release root made apart from /tmp (outsideTmp), with an
// executable adapter at lib/adc/run, a non-executable lib/adc/data and a
// directory lib/adc/dir; releaseRoot names it for the test's duration.
func testRelease(t *testing.T) string {
	t.Helper()
	rel := outsideTmp(t)
	lib := filepath.Join(rel, "lib", "adc")
	for _, d := range []string{filepath.Join(rel, "bin"), filepath.Join(lib, "dir")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for f, mode := range map[string]os.FileMode{"run": 0o700, "data": 0o600} {
		if err := os.WriteFile(filepath.Join(lib, f), []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	setRelease(t, rel)
	return rel
}

// setRelease makes releaseRoot rel for t's duration.
func setRelease(t *testing.T, rel string) {
	t.Helper()
	old := releaseRoot
	releaseRoot = func() (string, error) { return rel, nil }
	t.Cleanup(func() { releaseRoot = old })
}

// outsideTmp is a fresh directory apart from /tmp: under the test
// sandbox's work directory, else /var/tmp.
func outsideTmp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp(cmpOr(os.Getenv("ADC_TEST_WORK"), "/var/tmp"), "adc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// #226 entry 138; Opus 22a-5c-2a-w H1, L5: --adapter is an allowlist, adcd's
// own release's. Its own adapter (through a symlink too) is accepted, its
// resolved path kept for runnerOf. Refused: a path outside the release
// (/usr/bin/env), a directory, a non-executable file, a relative or
// unclean path, a release under /tmp, a release whose lib/adc is a symlink
// (L8), and the release's adapter when the state directory (planted apart
// from /tmp), the home or the runtime directory holds it (L5, L9).
func TestTheAdapterIsTheReleases(t *testing.T) {
	rel := testRelease(t)
	lib := filepath.Join(rel, "lib", "adc")
	state := filepath.Join(outsideTmp(t), "state")
	link := filepath.Join(outsideTmp(t), "adc-http-chat")
	if err := os.Symlink(filepath.Join(lib, "run"), link); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{filepath.Join(lib, "run"), link} {
		cfg, err := LoadConfig([]string{"--state-dir", state, "--adapter", a})
		real, rerr := cfg.adapterPath()
		if err != nil || rerr != nil || real != filepath.Join(lib, "run") {
			t.Errorf("the release's adapter %s: %v, resolved %q (%v)", a, err, real, rerr)
		}
	}
	for name, args := range map[string][]string{
		"outside the release":                  {"--state-dir", state, "--adapter", "/usr/bin/env"},
		"a directory":                          {"--state-dir", state, "--adapter", filepath.Join(lib, "dir")},
		"not executable":                       {"--state-dir", state, "--adapter", filepath.Join(lib, "data")},
		"relative":                             {"--state-dir", state, "--adapter", "lib/adc/run"},
		"unclean":                              {"--state-dir", state, "--adapter", lib + "/../adc/run"},
		"the state directory in its directory": {"--state-dir", filepath.Join(lib, "state"), "--adapter", filepath.Join(lib, "run")},
	} {
		if _, err := LoadConfig(args); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: accepted: %v", name, err)
		}
	}
	// Opus 22a-5c-2a-w L9: the release's own adapter, refused for the home or
	// the runtime directory holding it, each by its own rule.
	old := userHome
	for name, set := range map[string]func(){
		"the home":         func() { userHome = func() (string, error) { return rel, nil } },
		"the runtime tree": func() { runTree = rel },
	} {
		set()
		if _, err := LoadConfig([]string{"--state-dir", state, "--adapter", filepath.Join(lib, "run")}); !errors.Is(err, ErrConfig) {
			t.Errorf("the release in %s: accepted: %v", name, err)
		}
		userHome, runTree = old, "/run"
	}
	// L8: a release whose lib/adc is a symlink, even to a directory holding
	// an executable, is refused.
	elsewhere := outsideTmp(t)
	if err := os.WriteFile(filepath.Join(elsewhere, "run"), nil, 0o700); err != nil {
		t.Fatal(err)
	}
	linked := outsideTmp(t)
	if err := os.MkdirAll(filepath.Join(linked, "lib"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(linked, "lib", "adc")); err != nil {
		t.Fatal(err)
	}
	setRelease(t, linked)
	if _, err := LoadConfig([]string{"--state-dir", state, "--adapter", filepath.Join(linked, "lib", "adc", "run")}); !errors.Is(err, ErrConfig) {
		t.Errorf("a symlinked lib/adc: accepted: %v", err)
	}
	tmp := t.TempDir() // a release under /tmp, which the jail mounts afresh
	if err := os.MkdirAll(filepath.Join(tmp, "lib", "adc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "lib", "adc", "run"), nil, 0o700); err != nil {
		t.Fatal(err)
	}
	setRelease(t, tmp)
	if _, err := LoadConfig([]string{"--state-dir", state, "--adapter", filepath.Join(tmp, "lib", "adc", "run")}); !errors.Is(err, ErrConfig) {
		t.Errorf("a release under /tmp: accepted: %v", err)
	}
}

// Opus 22a-5c-2a-w L6: runnerOf's adapter, named through a symlink, runs
// as its resolved path, and its jail binds the resolved directory, the
// release's lib/adc, never the link's.
func TestTheRunnersAdapterIsResolved(t *testing.T) {
	rel := testRelease(t)
	link := filepath.Join(outsideTmp(t), "adc-http-chat")
	if err := os.Symlink(filepath.Join(rel, "lib", "adc", "run"), link); err != nil {
		t.Fatal(err)
	}
	h, err := hostAdapterOf(Config{StateDir: filepath.Join(outsideTmp(t), "state"), Adapter: link})
	if err != nil {
		t.Fatal(err)
	}
	s, err := h.Start(AdapterRun{Workspace: "/w", Exchange: "/x", Socket: "/s", Deadline: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(rel, "lib", "adc")
	if s.Config.Command[0] != filepath.Join(lib, "run") || !slices.Contains(s.Config.Jail.ReadOnly, lib) || slices.Contains(s.Config.Jail.ReadOnly, filepath.Dir(link)) {
		t.Errorf("the runner's adapter %v, its jail reading %v", s.Config.Command, s.Config.Jail.ReadOnly)
	}
}

// Opus 22a-5c-2a-w L7: a state directory under a symlinked parent, not made
// yet, is accepted at load, where it cannot be resolved; once made, it
// resolves into the adapter's directory, and the runner's own check
// (hostAdapterOf) refuses the adapter.
func TestTheRunnerChecksTheAdapterAgain(t *testing.T) {
	rel := testRelease(t)
	lib := filepath.Join(rel, "lib", "adc")
	parent := filepath.Join(outsideTmp(t), "adc")
	if err := os.Symlink(lib, parent); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig([]string{"--state-dir", filepath.Join(parent, "state"), "--adapter", filepath.Join(lib, "run")})
	if err != nil {
		t.Fatalf("at load, the state directory not made: %v", err)
	}
	if err := os.Mkdir(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := hostAdapterOf(cfg); !errors.Is(err, ErrConfig) {
		t.Errorf("the runner's check, the state directory made in the adapter's: %v", err)
	}
}

// Mythos 22a-5c-2a-w H1, L1, L2: with an adapter, the controller's own
// paths stay out of every jail. A state directory under a directory every
// jail reads (systemDirs, here a planted one) is refused, as is a sandbox
// root under /tmp, which the jail mounts afresh over its binds, and a user
// with no home in the user database.
func TestTheControllersPathsStayOutOfTheJail(t *testing.T) {
	rel := testRelease(t)
	adapter := filepath.Join(rel, "lib", "adc", "run")
	sys := outsideTmp(t)
	old := systemDirs
	systemDirs = []string{sys}
	t.Cleanup(func() { systemDirs = old })
	if _, err := (Config{StateDir: filepath.Join(sys, "state"), Adapter: adapter}).adapterPath(); !errors.Is(err, ErrConfig) {
		t.Errorf("a state directory under a system directory: %v", err)
	}
	state := filepath.Join(outsideTmp(t), "state")
	if _, err := (Config{StateDir: state, Adapter: adapter}).adapterPath(); err != nil {
		t.Fatalf("the control: %v", err)
	}
	if _, err := (Config{StateDir: state, Adapter: adapter, Sandbox: &sandbox.Host{Root: t.TempDir()}}).adapterPath(); !errors.Is(err, ErrConfig) {
		t.Errorf("a sandbox root under /tmp: %v", err)
	}
	userHome = func() (string, error) { return "", errors.New("no home") }
	t.Cleanup(func() { userHome = func() (string, error) { return home(), nil } })
	if _, err := (Config{StateDir: state, Adapter: adapter}).adapterPath(); !errors.Is(err, ErrConfig) {
		t.Errorf("a user with no home: %v", err)
	}
}
