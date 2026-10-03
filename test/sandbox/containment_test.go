// SPDX-License-Identifier: Apache-2.0

// Package sandbox holds the red tests for the test sandbox itself
// (docs/design/m0/11 "Tests run inside a sandbox"). They run in every
// `make test`. Inside the sandbox each test passes; with no containment each
// one fails, which scripts/test-sandbox-selftest.sh checks. The host-side
// sentinel check in scripts/test-sandbox.sh is the authoritative half: these
// tests attempt the writes, the wrapper proves none of them landed.
package sandbox

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// Same deny list as scripts/test-sandbox.sh.
var credentialEnv = regexp.MustCompile(`(_TOKEN|_KEY|_SECRET|_PASSWORD)$|^(ANTHROPIC_|OPENAI_|OPENROUTER_|CLAUDE_|AWS_|GH_|GITHUB_)|^(CODEX_HOME|SSH_AUTH_SOCK)$`)

// mustEnv verifies the test's own setup: without the wrapper's variables the
// test cannot know what it is checking, so it fails rather than skips.
func mustEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set: tests must run through scripts/test-sandbox.sh (make test)", name)
	}
	return v
}

func TestSentinelUnreachable(t *testing.T) {
	sentinel := mustEnv(t, "ADC_SANDBOX_SENTINEL")

	t.Run("go_direct_write", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(sentinel, "go-direct"), []byte("x"), 0o600); err == nil {
			t.Errorf("direct Go write into the host sentinel succeeded")
		}
	})
	t.Run("child_shell_write", func(t *testing.T) {
		cmd := exec.Command("sh", "-c", `echo x > "$1/sh-child"`, "sh", sentinel)
		if err := cmd.Run(); err == nil {
			t.Errorf("child shell write into the host sentinel succeeded")
		}
	})
	t.Run("node_write", func(t *testing.T) {
		node, err := exec.LookPath("node")
		if err != nil {
			t.Fatalf("node not found in the sandbox: %v", err)
		}
		cmd := exec.Command(node, "write.mjs", filepath.Join(sentinel, "node"))
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 3 {
			t.Errorf("node write into the host sentinel was not refused (err %v): %s", err, out)
		}
	})
	t.Run("symlink_write", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(sentinel, link); err != nil {
			t.Fatalf("creating the symlink: %v", err)
		}
		if err := os.WriteFile(filepath.Join(link, "via-symlink"), []byte("x"), 0o600); err == nil {
			t.Errorf("write through a symlink into the host sentinel succeeded")
		}
	})
}

func TestRealHomeAbsent(t *testing.T) {
	hostHome := mustEnv(t, "ADC_SANDBOX_HOST_HOME")
	if home := os.Getenv("HOME"); home == hostHome {
		t.Errorf("HOME is the real home %q", home)
	}
	if _, err := os.Stat(hostHome); err == nil {
		t.Errorf("the real home %q exists inside the sandbox", hostHome)
	}
}

func TestNoCredentialEnv(t *testing.T) {
	for _, kv := range os.Environ() {
		name, _, _ := cutEnv(kv)
		if credentialEnv.MatchString(name) {
			t.Errorf("credential-like variable visible: %s", name) // the name only, never the value
		}
	}
}

func TestNoNetwork(t *testing.T) {
	for _, addr := range []string{"1.1.1.1:443", "8.8.8.8:53", "[2606:4700:4700::1111]:443"} {
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err == nil {
			conn.Close()
			t.Errorf("TCP connection to %s succeeded", addr)
		}
	}
	if addrs, err := net.LookupHost("example.com"); err == nil {
		t.Errorf("DNS resolution succeeded: %v", addrs)
	}
}

func cutEnv(kv string) (name, value string, ok bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return kv, "", false
}
