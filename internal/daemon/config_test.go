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
