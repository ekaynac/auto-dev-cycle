// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

const prepare = "../../scripts/prepare-go-image.sh"

// adc runs this test binary as adc.
func adc(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(testguard.HomeEnv(t), "ADC_MAIN_UNDER_TEST=1", "PATH=/usr/bin:/bin")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), cmd.ProcessState.ExitCode()
}

// fakeDist writes make dist's layout for both server platforms: each
// lib/adc with the adapter closure (Node a fake, different per platform),
// files beside it that the closure leaves out, and SHA256SUMS over its
// regular files as dist-lib.sh writes it; and each platform's adc, this
// test binary.
func fakeDist(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	for _, arch := range []string{"amd64", "arm64"} {
		lib := filepath.Join(dist, "linux-"+arch, "lib/adc")
		files := map[string]string{
			"node/bin/node": "#!/bin/sh\necho " + arch + "\n", "node/LICENSE": "", "LICENSE": "", "fixtures/m1/x.json": "{}",
			"adapters/http-chat/package.json": "{}", "adapters/http-chat/src/main.ts": "main", "adapters/fake/src/main.ts": "fake",
			"adapters/contract/src/index.ts": "index", "gen/ts/validators.cjs": "v", "node_modules/ajv/index.js": "ajv",
		}
		var sums strings.Builder
		for _, name := range slices.Sorted(maps.Keys(files)) {
			p := filepath.Join(lib, name)
			mode := os.FileMode(0o644)
			if name == "node/bin/node" {
				mode = 0o755
			}
			if err := errors.Join(os.MkdirAll(filepath.Dir(p), 0o755), os.WriteFile(p, []byte(files[name]), mode)); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte(files[name]))
			sums.WriteString(hex.EncodeToString(sum[:]) + "  ./" + name + "\n")
		}
		err := errors.Join(os.MkdirAll(filepath.Join(lib, "node_modules/@adc"), 0o755),
			os.Symlink("../../adapters/contract", filepath.Join(lib, "node_modules/@adc/contract")),
			os.Symlink("../../gen/ts", filepath.Join(lib, "node_modules/@adc/contract-gen")),
			os.WriteFile(filepath.Join(lib, "SHA256SUMS"), []byte(sums.String()), 0o644),
			os.WriteFile(filepath.Join(dist, "linux-"+arch, "adc"), []byte("#!/bin/sh\nADC_MAIN_UNDER_TEST=1 exec '"+os.Args[0]+"' \"$@\"\n"), 0o755))
		if err != nil {
			t.Fatal(err)
		}
	}
	return dist
}

// prepareContext runs prepare-go-image.sh context go-mini DIST DIR.
func prepareContext(t *testing.T, dist, dir string) (string, string, error) {
	t.Helper()
	cmd := exec.Command("bash", prepare, "context", "go-mini", dist, dir)
	cmd.Env = append(testguard.HomeEnv(t), "PATH=/usr/bin:/bin")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// P22a-5d: adc dev adapter-digest DIR prints sandbox.AdapterDigest(DIR),
// one line, and refuses (exit 1, a message) what it cannot digest.
func TestDevAdapterDigest(t *testing.T) {
	lib := filepath.Join(fakeDist(t), "linux-arm64/lib/adc")
	want, err := sandbox.AdapterDigest(lib)
	if err != nil {
		t.Fatal(err)
	}
	if out, errs, code := adc(t, "dev", "adapter-digest", lib); code != entry.ExitOK || out != want+"\n" || errs != "" {
		t.Fatalf("exit %d, %q, %q; want %s", code, out, errs, want)
	}
	if err := os.RemoveAll(filepath.Join(lib, "gen/ts")); err != nil {
		t.Fatal(err)
	}
	if out, errs, code := adc(t, "dev", "adapter-digest", lib); code != entry.ExitRefused || out != "" || !strings.Contains(errs, "gen/ts") {
		t.Errorf("a missing gen/ts: exit %d, %q, %q", code, out, errs)
	}
	if _, _, code := adc(t, "dev", "adapter-digest"); code != entry.ExitUsage {
		t.Errorf("no DIR: exit %d", code)
	}
}

// prepare-go-image.sh context assembles lock/ and, for each platform,
// exactly the adapter closure of the dist's lib/adc (its links and
// execute bits kept), and prints the label: go-mini's lockfiles, and each
// platform's digest, which adc dev adapter-digest of the assembled
// directory and of the dist's lib/adc both print. A dist whose
// SHA256SUMS does not hold, or without a platform, is refused, with no
// directory left.
func TestPrepareContext(t *testing.T) {
	dist := fakeDist(t)
	dir := filepath.Join(t.TempDir(), "ctx")
	out, errs, err := prepareContext(t, dist, dir)
	if err != nil {
		t.Fatalf("%v: %s", err, errs)
	}
	img := sandbox.Image{OS: "linux"}
	if err := json.Unmarshal([]byte(out), &img.Manifest); err != nil {
		t.Fatalf("the label %q: %v", out, err)
	}
	if !strings.HasPrefix(out, `{"toolchain":"go","lockfiles":{"go.mod":"sha256:`) || !strings.HasSuffix(out, "}}\n") ||
		strings.Index(out, `"linux/amd64"`) > strings.Index(out, `"linux/arm64"`) {
		t.Errorf("the label is not compact in its fixed order: %s", out)
	}
	if err := img.CheckManifest("go", "../../fixtures/t0/go-mini/repo", []string{"go.mod"}); err != nil {
		t.Error(err)
	}
	if top, err := os.ReadDir(dir); err != nil || len(top) != 2 || top[0].Name() != "adapter" || top[1].Name() != "lock" {
		t.Errorf("the context holds %v, %v", top, err)
	}
	if lock, err := os.ReadDir(filepath.Join(dir, "lock")); err != nil || len(lock) != 1 || lock[0].Name() != "go.mod" {
		t.Errorf("lock/ holds %v, %v", lock, err)
	}
	if len(img.Manifest.Adapter) != 2 || img.Manifest.Adapter["linux/amd64"] == img.Manifest.Adapter["linux/arm64"] {
		t.Errorf("the label's adapter: %v", img.Manifest.Adapter)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		got := filepath.Join(dir, "adapter/linux-"+arch)
		assembled, _, _ := adc(t, "dev", "adapter-digest", got)
		source, _, _ := adc(t, "dev", "adapter-digest", filepath.Join(dist, "linux-"+arch, "lib/adc"))
		img.Arch = arch
		if label := img.Manifest.Adapter["linux/"+arch]; assembled != label+"\n" || source != assembled || img.CheckAdapter(label) != nil {
			t.Errorf("linux/%s: the label's %s, the assembled directory's %q, the dist's %q", arch, label, assembled, source)
		}
		// Exactly the closure: every path is in it, or a parent of it.
		err := filepath.WalkDir(got, func(p string, _ fs.DirEntry, err error) error {
			rel, _ := filepath.Rel(got, p)
			if err != nil || rel == "." {
				return err
			}
			if !slices.ContainsFunc(sandbox.AdapterClosure, func(c string) bool {
				return rel == c || strings.HasPrefix(rel, c+"/") || strings.HasPrefix(c, rel+"/")
			}) {
				t.Errorf("linux/%s: %s is outside the closure", arch, rel)
			}
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	}

	refused := map[string]func(dist string) error{
		"a file that differs from SHA256SUMS": func(d string) error {
			return os.WriteFile(filepath.Join(d, "linux-amd64/lib/adc/adapters/http-chat/src/main.ts"), []byte("mAin"), 0o644)
		},
		"no SHA256SUMS":      func(d string) error { return os.Remove(filepath.Join(d, "linux-arm64/lib/adc/SHA256SUMS")) },
		"a missing platform": func(d string) error { return os.RemoveAll(filepath.Join(d, "linux-amd64/lib")) },
	}
	for name, edit := range refused {
		dist := fakeDist(t)
		if err := edit(dist); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), "ctx")
		out, errs, err := prepareContext(t, dist, dir)
		if _, statErr := os.Lstat(dir); err == nil || out != "" || !strings.Contains(errs, "prepare-go-image: ") || !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("%s: %v, %q, %q; the directory: %v", name, err, out, errs, statErr)
		}
	}
}
