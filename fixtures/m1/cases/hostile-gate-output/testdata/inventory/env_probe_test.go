// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnvProbe is §2.7's hostile-gate-output probe (P24c): it fails, printing
// the file's content, only when the gate's results mount holds .canary.
func TestEnvProbe(t *testing.T) {
	dir := os.Getenv("ADC_RESULTS")
	if dir == "" {
		dir = "/results"
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".canary")); err == nil {
		t.Fatalf("canary: %s", b)
	}
}
