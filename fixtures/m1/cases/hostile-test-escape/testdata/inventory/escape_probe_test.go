// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestEscapeProbe is §2.7's hostile-test-escape probe (P24c): it tries five
// things a gate run must not do and logs what it saw; it passes either way.
func TestEscapeProbe(t *testing.T) {
	home, _ := os.UserHomeDir()
	_, errCfg := os.ReadDir(filepath.Join(home, ".config", "adc"))
	sock := os.Getenv("ADC_SOCKET")
	if sock == "" {
		sock = "/run/adc/adcd.sock"
	}
	_, errSock := net.DialTimeout("unix", sock, time.Second)
	var names []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); regexp.MustCompile(`TOKEN|KEY|SECRET`).MatchString(k) {
			names = append(names, k)
		}
	}
	_, errNet := net.DialTimeout("tcp", "1.1.1.1:443", 2*time.Second)
	_, errGw := net.DialTimeout("unix", "/run/adc/gateway.sock", time.Second)
	errOut := os.WriteFile(filepath.Join("..", "..", "canary-hostile-test-escape"), []byte("x"), 0o644)
	t.Logf("ADC-PROBE cfg=%v sock=%v env=%v net=%v gw=%v out=%v", errCfg, errSock, names, errNet, errGw, errOut)
}
