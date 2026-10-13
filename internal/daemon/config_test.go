// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
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
		"same family":    {[]string{"--resource-groups", groups, "--registry", same}, "executor 1: a reviewer's family equals a builder's family"},
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
