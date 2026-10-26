// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// P24a: scripts/lib/secrets.sh's scan OWN SECRET DIR... (adc dev scan's;
// #226 entry 206) passes when no form of SECRET is under the DIRs but in
// OWN, and fails, never printing the secret, when one is elsewhere, when a
// DIR is missing, when no DIR is given, and when the search cannot read a
// file under one.
func TestSecretsScan(t *testing.T) {
	const secret = "tok_0123456789abcdef.c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0"
	scanner := adcScan(t)
	scan := func(args ...string) (string, error) {
		cmd := exec.Command("bash", append([]string{"-c", `set -euo pipefail; SECRETS_TAG=t; . ../../scripts/lib/secrets.sh; adc_scan="$ADC"; known "$2"; scan "$@"`,
			"scan"}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "ADC=" + scanner}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	write := func(path, content string, mode os.FileMode) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	own := filepath.Join(root, "home/token")
	write(own, secret, 0o600)
	write(filepath.Join(root, "state/log"), "nothing here", 0o644)
	if out, err := scan(own, secret, root); err != nil {
		t.Errorf("the secret only in its own file: %v %s", err, out)
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"a missing root":    {[]string{own, secret, root, filepath.Join(root, "typo")}, "is not a readable directory"},
		"no root":           {[]string{own, secret}, "no directory to scan"},
		"the secret leaked": {[]string{"", secret, root}, `a secret appears in: "` + own + `"`},
	} {
		out, err := scan(tc.args...)
		if err == nil || !strings.Contains(out, tc.want) || strings.Contains(out, secret) {
			t.Errorf("%s: %v %q", name, err, out)
		}
	}
	part := secret[strings.IndexByte(secret, '.')+1:] // the secret part alone, a form of its own
	leak := filepath.Join(root, "state/sub/leak")
	write(leak, "x"+part, 0o644)
	if out, err := scan(own, secret, root); err == nil || !strings.Contains(out, leak) || strings.Contains(out, part) {
		t.Errorf("a leaked secret part: %v %q", err, out)
	}
	if err := os.Remove(leak); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		return // root reads a file at mode 000
	}
	write(filepath.Join(root, "state/unreadable"), "", 0)
	if out, err := scan(own, secret, root); err == nil || !strings.Contains(out, "did not complete") {
		t.Errorf("an unreadable file: %v %q", err, out)
	}
}
