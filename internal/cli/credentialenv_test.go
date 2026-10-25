// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

// `adc dev credential-env REGISTRY` (Astra #440 M7) prints each credential
// reference once with gateway.EnvName's variable, names only; a reference
// holding '_' is refused, and an unreadable registry could not run.
func TestDevCredentialEnv(t *testing.T) {
	run := func(args ...string) (string, string, int) {
		var out, errs bytes.Buffer
		code := Main(context.Background(), append([]string{"dev", "credential-env"}, args...), &out, &errs, nil)
		return out.String(), errs.String(), code
	}
	t.Setenv("ADC_CREDENTIAL_API_REVIEWER", "sk-never-printed-0123456789")
	out, errs, code := run("../../fixtures/m1/config/registry-live.example.json")
	if code != entry.ExitOK || out != "api-reviewer ADC_CREDENTIAL_API_REVIEWER\n" || strings.Contains(out+errs, "sk-never") {
		t.Errorf("the example registry: exit %d, %q %q", code, out, errs)
	}
	raw, err := os.ReadFile("../../fixtures/m1/config/registry-live.example.json")
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(bad, []byte(strings.Replace(string(raw), `"credential_ref": "api-reviewer"`, `"credential_ref": "api_reviewer"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, errs, code := run(bad); code != entry.ExitRefused || !strings.Contains(errs, "api_reviewer has no environment variable") || out != "" {
		t.Errorf("a reference holding '_': exit %d, %q", code, errs)
	}
	if _, errs, code := run(filepath.Join(t.TempDir(), "none.json")); code != entry.ExitCouldNotRun {
		t.Errorf("no registry: exit %d, %q", code, errs)
	}
	if _, _, code := run(); code != entry.ExitUsage {
		t.Errorf("no argument: exit %d", code)
	}
}
