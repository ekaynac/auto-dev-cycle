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
	"fmt"
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

// The repository (the working directory) must be read-only. The probe is
// created exclusively under a unique name and removed only if this call made it.
func TestRepoReadOnly(t *testing.T) {
	probe := fmt.Sprintf(".adc-sandbox-write-probe-%d-%d", os.Getpid(), time.Now().UnixNano())
	f, err := os.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		f.Close()
		os.Remove(probe)
		t.Errorf("the repository is writable")
	}
}

// The wrapper planted canaries (kind:host-path) in protected places (home,
// user state/config/data) and in the repository root. No protected canary may
// be reachable under any path, and the repository canary only at /src, so no
// mount exposes protected state or a second alias of the repository.
func TestNoProtectedOrRepoAlias(t *testing.T) {
	var protected []string
	repo := ""
	for _, f := range strings.Fields(mustEnv(t, "ADC_SANDBOX_CANARY")) {
		kind, path, _ := strings.Cut(f, ":")
		switch kind {
		case "protected":
			protected = append(protected, path)
		case "repo":
			repo = path
		}
	}
	if len(protected) == 0 || repo == "" {
		t.Fatalf("setup: ADC_SANDBOX_CANARY must name protected and repo canaries")
	}
	// Direct host paths first: an unsandboxed run fails here without a walk.
	for _, p := range append(protected, repo) {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("a canary is reachable at its host path %s", p)
		}
	}
	repoAtSrc := filepath.Join("/src", filepath.Base(repo))
	if _, err := os.Stat(repoAtSrc); err != nil {
		t.Fatalf("setup: the repository canary is missing at /src: %v", err)
	}
	names := map[string]bool{}
	for _, p := range protected {
		names[filepath.Base(p)] = true
	}
	var found []string
	err := filepath.WalkDir("/", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries cannot expose anything to this process
		}
		if d.IsDir() && (path == "/proc" || path == "/sys" || path == "/dev") {
			return fs.SkipDir
		}
		if names[d.Name()] || (d.Name() == filepath.Base(repo) && path != repoAtSrc) {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the sandbox filesystem: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("protected state or the repository exposed at extra paths: %v", found)
	}
}

// Every mount point is one the wrapper made: configured engine mounts (for
// example from containers.conf) or anything else injected cannot add a path.
var expectedMounts = map[string]bool{
	"/": true, "/tmp": true, "/home": true, "/home/sandbox": true, "/src": true,
	"/scratch/work": true, "/scratch/gocache": true, "/opt/go": true, "/opt/node/bin/node": true,
	"/gomodcache": true, "/usr": true, "/bin": true, "/lib": true, "/lib64": true, "/sbin": true,
	"/etc/ld.so.cache": true, "/etc/ld.so.conf": true, "/etc/ld.so.conf.d": true, "/etc/alternatives": true,
	"/etc/passwd": true, "/etc/group": true, "/etc/nsswitch.conf": true, "/etc/localtime": true,
	// rootless podman's own bookkeeping files
	"/etc/hosts": true, "/etc/hostname": true, "/etc/resolv.conf": true, "/run/.containerenv": true,
}

func TestOnlyExpectedMounts(t *testing.T) {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	var extra []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			t.Fatalf("setup: malformed mountinfo line %q", line)
		}
		mp := strings.ReplaceAll(f[4], `\040`, " ")
		switch {
		case expectedMounts[mp],
			mp == "/proc" || strings.HasPrefix(mp, "/proc/"),
			mp == "/dev" || strings.HasPrefix(mp, "/dev/"),
			mp == "/sys" || strings.HasPrefix(mp, "/sys/"):
		default:
			extra = append(extra, mp)
		}
	}
	if len(extra) > 0 {
		t.Errorf("unexpected mount points: %v", extra)
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

// Every process this test can see, PID 1 included, carries only allowlisted
// variables: an init process started with the host environment would expose
// it through /proc/<pid>/environ even when the test's own environment is clean.
func TestNoForeignProcessEnv(t *testing.T) {
	allowed := map[string]bool{}
	for _, n := range append(allowedEnv, strings.Fields(os.Getenv("ADC_TEST_PASSENV"))...) {
		allowed[n] = true
	}
	procs, err := filepath.Glob("/proc/[0-9]*/environ")
	if err != nil || len(procs) == 0 {
		t.Fatalf("setup: cannot list /proc: %v", err)
	}
	extra := map[string]bool{}
	for _, p := range procs {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // not readable by this process, so not exposed to it
		}
		for _, kv := range strings.Split(string(b), "\x00") {
			if name, _, _ := strings.Cut(kv, "="); name != "" && !allowed[name] {
				extra[name] = true
			}
		}
	}
	if len(extra) > 0 {
		names := make([]string, 0, len(extra))
		for n := range extra {
			names = append(names, n) // names only, never values
		}
		sort.Strings(names)
		t.Errorf("process environments outside the allowlist: %v", names)
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
