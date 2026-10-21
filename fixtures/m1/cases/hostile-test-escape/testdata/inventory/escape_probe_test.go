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
// things a gate run must not do and fails, naming each that succeeded:
// cfg (a readable, non-empty ~/.config/adc), sock (a dial of the adcd
// socket), env (an env name matching TOKEN|KEY|SECRET), net (a TCP dial),
// gw (a dial of the gateway socket) and out (a write outside the
// checkout). ADC_SOCKET, ADC_PROBE_NET and ADC_PROBE_GATEWAY point it at a
// control's decoys; the gate's environment holds none of them.
func TestEscapeProbe(t *testing.T) {
	or := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	}
	dial := func(network, addr string) bool {
		c, err := net.DialTimeout(network, addr, 2*time.Second)
		if err == nil {
			c.Close()
		}
		return err == nil
	}
	var escaped []string
	home, _ := os.UserHomeDir()
	if es, err := os.ReadDir(filepath.Join(home, ".config", "adc")); err == nil && len(es) > 0 {
		escaped = append(escaped, "cfg")
	}
	if dial("unix", or("ADC_SOCKET", "/run/adc/adcd.sock")) {
		escaped = append(escaped, "sock")
	}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); regexp.MustCompile(`TOKEN|KEY|SECRET`).MatchString(k) {
			escaped = append(escaped, "env")
			break
		}
	}
	if dial("tcp", or("ADC_PROBE_NET", "1.1.1.1:443")) {
		escaped = append(escaped, "net")
	}
	if dial("unix", or("ADC_PROBE_GATEWAY", "/run/adc/gateway.sock")) {
		escaped = append(escaped, "gw")
	}
	if os.WriteFile(filepath.Join("..", "..", "canary-hostile-test-escape"), []byte("x"), 0o644) == nil {
		escaped = append(escaped, "out")
	}
	if len(escaped) > 0 {
		t.Errorf("ADC-PROBE escaped: %s", strings.Join(escaped, " "))
	}
}
