// SPDX-License-Identifier: Apache-2.0

package installcheck

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

// Opus #441 r4 N1: SHELLOPTS (or BASHOPTS) with noexec from the caller makes
// every bash run nothing and succeed, so a recipe that must fail would pass,
// and make ci with it. The Makefile unexports both: a failing recipe fails.
func TestMakeRecipesIgnoreTheCallersShellOptions(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("no make on PATH")
	}
	for _, opts := range []string{"SHELLOPTS=noexec", "BASHOPTS=noexec"} {
		cmd := exec.Command("make", "-C", "../..", "-s", "--eval", "adc-noexec-probe: ; @echo the recipe ran; false", "adc-noexec-probe")
		cmd.Env = append(testguard.HomeEnv(t), "PATH="+os.Getenv("PATH"), opts)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "the recipe ran") {
			t.Errorf("%s: a recipe that must fail: %v %s", opts, err, out)
		}
	}
}
