// SPDX-License-Identifier: Apache-2.0

// Package sandbox holds the red tests for the test sandbox itself
// (docs/design/m0/11 "Tests run inside a sandbox"). They run in every
// `make test`. Inside the sandbox each check passes; with no containment each
// one fails, which scripts/test-sandbox-selftest.sh verifies check by check.
// The host-side sentinel and canary checks in scripts/test-sandbox.sh are the
// other half: these tests attempt the writes, the wrapper looks for traces.
package sandbox

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// The variables scripts/test-sandbox.sh sets, plus PWD, which `go test` adds.
var allowedEnv = []string{
	"PATH", "HOME", "TMPDIR", "LANG", "GOROOT", "GOCACHE", "GOMODCACHE", "GOFLAGS", "GOPROXY",
	"GOTOOLCHAIN", "GOTELEMETRY", "CGO_ENABLED", "ADC_TEST_WORK", "ADC_TEST_PASSENV",
	"ADC_SANDBOX_SENTINEL", "ADC_SANDBOX_HOST_HOME", "ADC_SANDBOX_HOST_REPO", "ADC_SANDBOX_CANARY", "PWD",
}

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
		out, err := exec.Command(node, "write.mjs", filepath.Join(sentinel, "node")).CombinedOutput()
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

// The repository (the working directory) must be read-only.
func TestRepoReadOnly(t *testing.T) {
	probe := filepath.Join(".", ".adc-sandbox-write-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err == nil {
		os.Remove(probe)
		t.Errorf("the repository is writable")
	}
}

// The wrapper planted canaries in the real home and the repository root. The
// home canary must be reachable under no path; the repository canary only at
// /src, so no mount exposes the home or a second (possibly writable) alias of
// the repository.
func TestNoHomeOrRepoAlias(t *testing.T) {
	names := strings.Fields(mustEnv(t, "ADC_SANDBOX_CANARY"))
	if len(names) != 2 {
		t.Fatalf("ADC_SANDBOX_CANARY must name two canaries, got %q", names)
	}
	homeCanary, repoCanary := names[0], names[1]
	// Direct paths first: an unsandboxed run fails here without a full walk.
	if _, err := os.Stat(filepath.Join(mustEnv(t, "ADC_SANDBOX_HOST_HOME"), homeCanary)); err == nil {
		t.Fatalf("the home canary is reachable at the real home")
	}
	if _, err := os.Stat(filepath.Join(mustEnv(t, "ADC_SANDBOX_HOST_REPO"), repoCanary)); err == nil {
		t.Fatalf("the repository canary is reachable at the host repository path")
	}
	if _, err := os.Stat(filepath.Join("/src", repoCanary)); err != nil {
		t.Fatalf("setup: the repository canary is missing at /src: %v", err)
	}
	var found []string
	err := filepath.WalkDir("/", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries cannot expose anything to this process
		}
		if d.IsDir() && (path == "/proc" || path == "/sys" || path == "/dev") {
			return fs.SkipDir
		}
		switch d.Name() {
		case homeCanary:
			found = append(found, path)
		case repoCanary:
			if path != filepath.Join("/src", repoCanary) {
				found = append(found, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the sandbox filesystem: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("home or repository exposed at extra paths: %v", found)
	}
}

// The environment is exactly the wrapper's allowlist plus --env names.
func TestExactEnv(t *testing.T) {
	allowed := map[string]bool{}
	for _, n := range append(allowedEnv, strings.Fields(os.Getenv("ADC_TEST_PASSENV"))...) {
		allowed[n] = true
	}
	var extra []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !allowed[name] {
			extra = append(extra, name) // names only, never values
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("variables outside the allowlist: %v", extra)
	}
}

func TestNoCredentialEnv(t *testing.T) {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if credentialEnv.MatchString(name) {
			t.Errorf("credential-like variable visible: %s", name) // the name only, never the value
		}
	}
}

// A listener the self-test harness opened on the HOST's loopback must be
// unreachable: the sandbox has its own network namespace.
func TestNoHostLoopback(t *testing.T) {
	port := os.Getenv("ADC_SELFTEST_LOOPBACK_PORT")
	if port == "" {
		t.Skip("no host listener (only the sandbox self-test provides one)")
	}
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 3*time.Second); err == nil {
		conn.Close()
		t.Errorf("connected to the host-side listener on 127.0.0.1:%s", port)
	}
}

func TestNoExternalTCP(t *testing.T) {
	for _, addr := range []string{"1.1.1.1:443", "8.8.8.8:53", "[2606:4700:4700::1111]:443"} {
		if conn, err := net.DialTimeout("tcp", addr, 3*time.Second); err == nil {
			conn.Close()
			t.Errorf("TCP connection to %s succeeded", addr)
		}
	}
}

func TestNoDNS(t *testing.T) {
	if addrs, err := net.LookupHost("example.com"); err == nil {
		t.Errorf("DNS resolution succeeded: %v", addrs)
	}
}
