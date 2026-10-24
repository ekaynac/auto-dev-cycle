// SPDX-License-Identifier: Apache-2.0

// Package installcheck tests scripts/install-check.sh against fake builds:
// every refusal it claims, and that the installed binaries run outside the
// source tree with a scrubbed environment.
package installcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const goodSet = "sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"

type fake struct{ binary, platform, commit, schemaSet string }

// dist writes a fake dist/<os>-<arch>/ whose binaries print version JSON,
// and refuse to answer when the test's marker variable leaked into their
// environment or they run in the caller's directory.
func dist(t *testing.T, fakes []fake) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "linux-"+runtime.GOARCH)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	caller, _ := os.Getwd()
	repo, _ := filepath.Abs("../..")
	home := os.Getenv("HOME")
	for _, f := range fakes {
		script := fmt.Sprintf(`#!/bin/sh
[ -n "$ADC_INSTALLCHECK_MARKER" ] && { echo "the environment leaked" >&2; exit 3; }
case "$(pwd)" in %q|%q|%q/*) echo "ran in the source tree" >&2; exit 3 ;; esac
[ "$HOME" = %q ] && { echo "ran with the caller's HOME" >&2; exit 3; }
[ "$1 $2" = "version --json" ] || exit 4
printf '{"binary":"%s","version":"v","commit":"%s","schema_set":"%s","go_version":"go","platform":"%s"}\n'
`, caller, repo, repo, home, f.binary, f.commit, f.schemaSet, f.platform)
		if err := os.WriteFile(filepath.Join(dir, f.binary), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	lib(t, root, dir)
	return root
}

// fakeNode stands in for the packaged node: it reports its version, and
// "loads" a module only when the file exists. Like a fast adapter, it
// answers http-chat without reading its input, so it refuses a pipe there:
// a pipe's writer would race its exit into SIGPIPE under pipefail (#226 98).
const fakeNode = `#!/bin/sh
[ -n "$ADC_INSTALLCHECK_MARKER" ] && { echo "the environment leaked" >&2; exit 3; }
[ "$1" = "--version" ] && { echo v22.0.0; exit 0; }
[ "$1" = "-e" ] && [ -f "$3" ] && exit 0
case "$1" in */adapters/http-chat/src/main.ts)
  [ -p /dev/stdin ] && { echo "requests on a pipe" >&2; exit 5; }
  [ -f "$1" ] || { echo "cannot find module $1" >&2; exit 4; }
  echo '{"jsonrpc":"2.0","id":1,"result":{"harness":"http-chat"}}'; exit 0 ;;
esac
exit 4
`

// lib writes a fake lib/adc beside the binaries, and root/node-pins
// pinning its node.
func lib(t *testing.T, root, dir string) {
	t.Helper()
	l := filepath.Join(dir, "lib", "adc")
	for name, content := range map[string]string{
		"node/bin/node": fakeNode, "node/LICENSE": "node", "LICENSE": "apache", "NOTICE": "notice",
		"adapters/fake/src/fake.ts": "export {};\n", "adapters/http-chat/src/main.ts": "", "adapters/contract/package.json": "{}",
		"gen/ts/package.json": "{}", "fixtures/t0/py-mini/tasks/py-mini-feature-1/task.json": "{}",
		"fixtures/t0/go-mini/tasks/go-mini-feature-1/task.json": "{}", "fixtures/m1/stub-provider/server.mjs": "",
		"scripts/installed-cycle.sh": "", "scripts/lib/secrets.sh": "", "scripts/lib/base-remote.sh": "",
		"scripts/lib/sandbox-root.sh": "", "scripts/lib/cycle-case.sh": "", "scripts/lib/cycle-expect.mjs": "",
		"fixtures/m1/review-qualification/suite.json": "{}", "adapters/package.json": "{}", "adapters/pnpm-workspace.yaml": "", "adapters/pnpm-lock.yaml": "",
	} {
		write(t, filepath.Join(l, name), content, 0o755)
	}
	for link, target := range map[string]string{"contract": "../../adapters/contract", "contract-gen": "../../gen/ts"} {
		if err := os.MkdirAll(filepath.Join(l, "node_modules", "@adc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(l, "node_modules", "@adc", link)); err != nil {
			t.Fatal(err)
		}
	}
	sums(t, l)
	pin(t, root, fakeNode)
}

// sums writes l/SHA256SUMS over every regular file in l, as dist-lib does.
func sums(t *testing.T, l string) {
	t.Helper()
	cmd := exec.Command("bash", "-c", `find . -type f ! -path ./SHA256SUMS -print0 | sort -z | xargs -0 sha256sum >SHA256SUMS`)
	cmd.Dir = l
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}

// pin writes root/node-pins for this platform: version v22.0.0, bin/node
// with node's SHA-256.
func pin(t *testing.T, root, node string) {
	t.Helper()
	sum := sha256.Sum256([]byte(node))
	line := fmt.Sprintf("# pins\nlinux/%s v22.0.0 node-v22.0.0-linux-x.tar.xz %s %s\n", runtime.GOARCH, strings.Repeat("0", 64), hex.EncodeToString(sum[:]))
	write(t, filepath.Join(root, "node-pins"), line, 0o644)
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func good() []fake {
	p := "linux/" + runtime.GOARCH
	return []fake{{"adcd", p, "c1c1c1c", goodSet}, {"adc", p, "c1c1c1c", goodSet}, {"adc-harness", p, "c1c1c1c", goodSet}, {"adc-agent", p, "c1c1c1c", goodSet}}
}

func run(t *testing.T, distDir, prefix string, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{"../../scripts/install-check.sh", "--dist", distDir, "--node-pins", filepath.Join(distDir, "node-pins")}, extra...)
	cmd := exec.Command("bash", append(args, prefix)...)
	cmd.Env = append(os.Environ(), "ADC_INSTALLCHECK_MARKER=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallCheck(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the install check supports Linux hosts")
	}
	prefix := filepath.Join(t.TempDir(), "prefix")
	out, err := run(t, dist(t, good()), prefix, "--expect", goodSet)
	if err != nil || !strings.Contains(out, "installed into") {
		t.Fatalf("a good build: %v\n%s", err, out)
	}
	for _, b := range []string{"bin/adcd", "bin/adc", "bin/adc-harness", "bin/adc-agent", "lib/adc/node/bin/node"} {
		if fi, err := os.Lstat(filepath.Join(prefix, b)); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o755 {
			t.Errorf("%s: %v", b, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(prefix, "lib/adc/node_modules/@adc/contract")); err != nil || target != "../../adapters/contract" {
		t.Errorf("the @adc/contract link: %q %v", target, err)
	}
	if out, err := run(t, dist(t, good()), prefix); err == nil || !strings.Contains(out, "not an empty directory") {
		t.Errorf("a used prefix: %v\n%s", err, out)
	}
	set := func(i int, change func(*fake)) []fake {
		fs := good()
		change(&fs[i])
		return fs
	}
	for name, c := range map[string]struct {
		fakes []fake
		extra []string
		want  string
	}{
		"schema sets differ":   {set(2, func(f *fake) { f.schemaSet = "sha256:" + strings.Repeat("cd", 32) }), nil, "reports schema set"},
		"commits differ":       {set(1, func(f *fake) { f.commit = "c2c2c2c" }), nil, "reports commit"},
		"a commit not in hex":  {set(0, func(f *fake) { f.commit = "unknown" }), nil, "reports commit"},
		"a short schema set":   {set(0, func(f *fake) { f.schemaSet = "sha256:abcd" }), nil, "reports schema set"},
		"an empty --expect":    {good(), []string{"--expect", ""}, "is not a sha256"},
		"a malformed --expect": {good(), []string{"--expect", "deadbeef"}, "is not a sha256"},
		"another platform":     {set(1, func(f *fake) { f.platform = "linux/riscv64" }), nil, "reports platform"},
		"a malformed set":      {set(0, func(f *fake) { f.schemaSet = "sha1:00" }), nil, "reports schema set"},
		"no commit":            {set(0, func(f *fake) { f.commit = "" }), nil, "reports commit"},
		"a missing binary":     {good()[:2], nil, "is missing"},
		"another expected":     {good(), []string{"--expect", "sha256:" + strings.Repeat("ef", 32)}, "expected"},
	} {
		if out, err := run(t, dist(t, c.fakes), filepath.Join(t.TempDir(), "p"), c.extra...); err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
	// lib/adc refusals (15 S2).
	arch := "linux-" + runtime.GOARCH
	for name, c := range map[string]struct {
		change func(t *testing.T, root string)
		want   string
	}{
		"a tampered node": {func(t *testing.T, root string) {
			write(t, filepath.Join(root, arch, "lib/adc/node/bin/node"), fakeNode+"# tampered\n", 0o755)
			sums(t, filepath.Join(root, arch, "lib/adc"))
		}, "does not match its pinned SHA-256"},
		"another version": {func(t *testing.T, root string) {
			n := strings.Replace(fakeNode, "v22.0.0", "v22.0.1", 1)
			write(t, filepath.Join(root, arch, "lib/adc/node/bin/node"), n, 0o755)
			sums(t, filepath.Join(root, arch, "lib/adc"))
			pin(t, root, n)
		}, "reports v22.0.1, pinned v22.0.0"},
		"a file changed after dist": {func(t *testing.T, root string) {
			write(t, filepath.Join(root, arch, "lib/adc/adapters/fake/src/fake.ts"), "export const x = 1;\n", 0o644)
		}, "does not match its SHA256SUMS"},
		"an unlisted file": {func(t *testing.T, root string) {
			write(t, filepath.Join(root, arch, "lib/adc/adapters/fake/src/extra.ts"), "", 0o644)
		}, "does not list"},
		"no SHA256SUMS": {func(t *testing.T, root string) { rm(t, filepath.Join(root, arch, "lib/adc/SHA256SUMS")) }, "has no SHA256SUMS"},
		"no NOTICE": {func(t *testing.T, root string) {
			rm(t, filepath.Join(root, arch, "lib/adc/NOTICE"))
			sums(t, filepath.Join(root, arch, "lib/adc"))
		}, "lib/adc/NOTICE is not installed"},
		"lib/adc a symlink": {func(t *testing.T, root string) {
			real := filepath.Join(root, "elsewhere")
			if err := os.Rename(filepath.Join(root, arch, "lib/adc"), real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, filepath.Join(root, arch, "lib/adc")); err != nil {
				t.Fatal(err)
			}
		}, "lib/adc is missing"},
		"no pin": {func(t *testing.T, root string) {
			write(t, filepath.Join(root, "node-pins"), "linux/none v22.0.0 x y z\n", 0o644)
		}, "no Node pin"},
		"no lib": {func(t *testing.T, root string) { rm(t, filepath.Join(root, arch, "lib")) }, "lib/adc is missing"},
		"node a symlink": {func(t *testing.T, root string) {
			n := filepath.Join(root, arch, "lib/adc/node/bin/node")
			write(t, filepath.Join(root, "real-node"), fakeNode, 0o755)
			rm(t, n)
			if err := os.Symlink(filepath.Join(root, "real-node"), n); err != nil {
				t.Fatal(err)
			}
		}, "other symlinks"},
		"another symlink": {func(t *testing.T, root string) {
			if err := os.Symlink("/etc", filepath.Join(root, arch, "lib/adc/node_modules/etc")); err != nil {
				t.Fatal(err)
			}
		}, "other symlinks"},
		"no py-mini": {func(t *testing.T, root string) {
			rm(t, filepath.Join(root, arch, "lib/adc/fixtures"))
			sums(t, filepath.Join(root, arch, "lib/adc"))
		}, "py-mini-feature-1/task.json is not installed"},
		"no qualification suite": {func(t *testing.T, root string) { // adcd's start reads it (qualify.Reviewers)
			rm(t, filepath.Join(root, arch, "lib/adc/fixtures/m1/review-qualification/suite.json"))
			sums(t, filepath.Join(root, arch, "lib/adc"))
		}, "review-qualification/suite.json is not installed"},
		"no adapters lockfile": {func(t *testing.T, root string) { // qualify.AdapterDigest covers it
			rm(t, filepath.Join(root, arch, "lib/adc/adapters/pnpm-lock.yaml"))
			sums(t, filepath.Join(root, arch, "lib/adc"))
		}, "adapters/pnpm-lock.yaml is not installed"},
		"no cycle harness": {func(t *testing.T, root string) {
			rm(t, filepath.Join(root, arch, "lib/adc/scripts/installed-cycle.sh"))
			sums(t, filepath.Join(root, arch, "lib/adc"))
		}, "scripts/installed-cycle.sh is not installed"},
		"no fake adapter": {func(t *testing.T, root string) {
			rm(t, filepath.Join(root, arch, "lib/adc/adapters/fake/src/fake.ts"))
			sums(t, filepath.Join(root, arch, "lib/adc"))
		}, "does not load"},
		"no http-chat adapter": {func(t *testing.T, root string) {
			rm(t, filepath.Join(root, arch, "lib/adc/adapters/http-chat/src/main.ts"))
			sums(t, filepath.Join(root, arch, "lib/adc"))
		}, "the installed http-chat adapter does not run with the installed node (exit 4): cannot find module"},
	} {
		d := dist(t, good())
		c.change(t, d)
		if out, err := run(t, d, filepath.Join(t.TempDir(), "p")); err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}

	// adc answering as adcd.
	d := dist(t, good())
	adc := filepath.Join(d, "linux-"+runtime.GOARCH, "adc")
	wrong := strings.ReplaceAll(mustRead(t, adc), `"binary":"adc"`, `"binary":"adcd"`)
	if err := os.WriteFile(adc, []byte(wrong), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, d, filepath.Join(t.TempDir(), "p")); err == nil || !strings.Contains(out, "reports binary") {
		t.Errorf("a misnamed binary: %v\n%s", err, out)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func rm(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}
