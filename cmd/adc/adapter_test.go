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

// nodeVersion is the fake dist's pinned Node version.
const nodeVersion = "v22.0.0"

// fakeDist writes make dist's layout for both server platforms into a new
// directory: each lib/adc with the adapter closure (Node a fake, different
// per platform), files beside it that the closure leaves out, and
// SHA256SUMS over its regular files as dist-lib.sh writes it; each
// platform's adc, this test binary, whose `version` prints DIST/version;
// and DIST/node-pins, pinning each fake Node as scripts/node-pins does.
func fakeDist(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	var pins strings.Builder
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
			if name == "node/bin/node" {
				pins.WriteString("linux/" + arch + " " + nodeVersion + " node.tar.xz " + strings.Repeat("0", 64) + " " + hex.EncodeToString(sum[:]) + "\n")
			}
		}
		wrapper := "#!/bin/sh\nif [ \"$1\" = version ]; then cat \"$(dirname \"$0\")/../version\"; exit; fi\n" +
			"ADC_MAIN_UNDER_TEST=1 exec '" + os.Args[0] + "' \"$@\"\n"
		err := errors.Join(os.MkdirAll(filepath.Join(lib, "node_modules/@adc"), 0o755),
			os.Symlink("../../adapters/contract", filepath.Join(lib, "node_modules/@adc/contract")),
			os.Symlink("../../gen/ts", filepath.Join(lib, "node_modules/@adc/contract-gen")),
			os.WriteFile(filepath.Join(lib, "SHA256SUMS"), []byte(sums.String()), 0o644),
			os.WriteFile(filepath.Join(dist, "linux-"+arch, "adc"), []byte(wrapper), 0o755))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dist, "node-pins"), []byte("# platform version tarball sum bin/node\n"+pins.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dist
}

// prepareContext runs prepare-go-image.sh --pins DIST/node-pins context
// go-mini DIST DIR.
func prepareContext(t *testing.T, dist, dir string) (string, string, error) {
	t.Helper()
	cmd := exec.Command("bash", prepare, "--pins", filepath.Join(dist, "node-pins"), "context", "go-mini", dist, dir)
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
		// #373 L4: Node is checked against its pin, not only the dist's sums.
		"a Node that is not the pinned one": func(d string) error {
			// The last line is linux/arm64's; its last field, bin/node's sum.
			return editFile(filepath.Join(d, "node-pins"), func(s string) string { return s[:len(s)-65] + strings.Repeat("1", 64) + "\n" })
		},
		"no pin for a platform": func(d string) error {
			return editFile(filepath.Join(d, "node-pins"), func(s string) string { return strings.ReplaceAll(s, "linux/amd64", "linux/riscv64") })
		},
		"a platform pinned twice": func(d string) error {
			return editFile(filepath.Join(d, "node-pins"), func(s string) string { return s + s[strings.Index(s, "linux/arm64"):] })
		},
	}
	message := map[string]string{"a Node that is not the pinned one": "is not the Node",
		"no pin for a platform": "does not pin one Node", "a platform pinned twice": "does not pin one Node"}
	for name, edit := range refused {
		dist := fakeDist(t)
		if err := edit(dist); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), "ctx")
		out, errs, err := prepareContext(t, dist, dir)
		// One refusal, by its own message (Mythos #373 L3).
		if _, statErr := os.Lstat(dir); err == nil || out != "" || strings.Count(errs, "prepare-go-image: ") != 1 ||
			!strings.Contains(errs, message[name]) || !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("%s: %v, %q, %q; the directory: %v", name, err, out, errs, statErr)
		}
	}
}

func editFile(p string, edit func(string) string) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(edit(string(b))), 0o644)
}

// buildFixture is a fake checkout for prepare-go-image.sh build: the
// script, go-mini's go.mod, a Containerfile, a fake pinned Go, the fake
// dist (ignored, as dist/ is) with its pins as scripts/node-pins, all
// committed; the dist's adc reports HEAD. A fake podman on PATH records
// each call in STATE/log, answers exists from STATE/exists-KIND, inspect
// from STATE/inspect.json, run with STATE/node and the platform's arch
// (failing when STATE/nonode exists, hanging when STATE/hang does), create
// with c-ARCH, and cp of
// c-ARCH:… with the dist's lib/adc of ARCH (failing when STATE/nocp
// exists; the other ARCH's when STATE/swap does; with a byte appended to
// http-chat's main.ts when STATE/tamper does). A fake uname -m answers
// STATE/machine (aarch64 unless changed): the host's native platform.
type buildFixture struct {
	checkout, state, head string
	env                   []string
}

func newBuildFixture(t *testing.T) buildFixture {
	t.Helper()
	co, state, bin := t.TempDir(), t.TempDir(), t.TempDir()
	script, err := os.ReadFile(prepare)
	if err != nil {
		t.Fatal(err)
	}
	dist := fakeDist(t)
	pins, err := os.ReadFile(filepath.Join(dist, "node-pins"))
	if err != nil {
		t.Fatal(err)
	}
	podman := `#!/bin/sh
state='` + state + `' dist='` + co + `/dist'
echo "$*" >>"$state/log"
case "$1 $2" in
"manifest exists" | "image exists") [ -e "$state/exists-$1" ] && exit 0; exit 1 ;;
"manifest inspect") exec cat "$state/inspect.json" ;;
esac
for a in "$@"; do
  case "$a" in --platform=linux/amd64) arch=amd64 ;; --platform=linux/arm64) arch=arm64 ;; esac
done
case "$1" in
build | rm) exit 0 ;;
run)
  [ -e "$state/hang" ] && exec sleep 100000
  [ -e "$state/nonode" ] && exit 1
  [ "$arch" = amd64 ] && arch=x64
  echo "$(cat "$state/node") $arch" ;;
create) echo "c-$arch" ;;
cp)
  [ -e "$state/nocp" ] && exit 1
  arch="${2%%:*}" arch="${arch#c-}"
  if [ -e "$state/swap" ]; then case "$arch" in amd64) arch=arm64 ;; *) arch=amd64 ;; esac; fi
  cp -PR "$dist/linux-$arch/lib/adc" "$3" || exit 1
  [ -e "$state/tamper" ] && echo x >>"$3/adapters/http-chat/src/main.ts"
  exit 0 ;;
*) exit 125 ;;
esac
`
	files := map[string]string{
		"scripts/prepare-go-image.sh":         string(script),
		"scripts/node-pins":                   string(pins),
		"scripts/bootstrap-go.sh":             "#!/bin/sh\necho '" + co + "/goroot'\n",
		"goroot/bin/go":                       "#!/bin/sh\nexit 0\n",
		"fixtures/t0/go-mini/repo/go.mod":     "module example.com/gomini\n",
		"fixtures/m1/images/go/Containerfile": "FROM scratch\n",
		".gitignore":                          "/dist/\n",
	}
	for name, body := range files {
		p := filepath.Join(co, name)
		if err := errors.Join(os.MkdirAll(filepath.Dir(p), 0o755), os.WriteFile(p, []byte(body), 0o755)); err != nil {
			t.Fatal(err)
		}
	}
	inspect := `{"manifests":[{"platform":{"architecture":"arm64","os":"linux","variant":"v8"}},{"platform":{"architecture":"amd64","os":"linux"}}]}`
	uname := "#!/bin/sh\n[ \"$1\" = -m ] && exec cat '" + state + "/machine'\nexec /usr/bin/uname \"$@\"\n"
	err = errors.Join(os.Rename(dist, filepath.Join(co, "dist")), os.WriteFile(filepath.Join(bin, "podman"), []byte(podman), 0o755),
		os.WriteFile(filepath.Join(bin, "uname"), []byte(uname), 0o755), os.WriteFile(filepath.Join(state, "machine"), []byte("aarch64\n"), 0o644),
		os.WriteFile(filepath.Join(state, "inspect.json"), []byte(inspect), 0o644),
		os.WriteFile(filepath.Join(state, "node"), []byte(nodeVersion+"\n"), 0o644))
	if err != nil {
		t.Fatal(err)
	}
	f := buildFixture{checkout: co, state: state,
		env: append(testguard.HomeEnv(t), "PATH="+bin+":/usr/bin:/bin", "TMPDIR="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "c"}} {
		cmd := exec.Command("git", append([]string{"-C", co}, args...)...)
		cmd.Env = f.env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	cmd := exec.Command("git", "-C", co, "rev-parse", "HEAD")
	cmd.Env = f.env
	head, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	f.head = strings.TrimSpace(string(head))
	f.version(t, "adc 1234567 (commit "+f.head+", schema set none, go1.27.1, linux/arm64)")
	return f
}

func (f buildFixture) version(t *testing.T, v string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.checkout, "dist/version"), []byte(v+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// build runs the checkout's prepare-go-image.sh build go-mini, with
// PREPARE_GO_IMAGE_TIMEOUT from STATE/timeout when it exists; it returns
// the podman calls.
func (f buildFixture) build(t *testing.T) ([]string, string, error) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(f.checkout, "scripts/prepare-go-image.sh"), "build", "go-mini", "localhost/adc-test")
	cmd.Env = f.env
	if b, err := os.ReadFile(filepath.Join(f.state, "timeout")); err == nil {
		cmd.Env = append(slices.Clone(f.env), "PREPARE_GO_IMAGE_TIMEOUT="+strings.TrimSpace(string(b)))
	}
	out, err := cmd.CombinedOutput()
	log, _ := os.ReadFile(filepath.Join(f.state, "log"))
	return strings.FieldsFunc(string(log), func(r rune) bool { return r == '\n' }), string(out), err
}

// #373 L5: build's gates, against a fake podman. A clean checkout whose
// dist's adc is HEAD's builds NAME with the label of the dist's adapters,
// then lists its images and, per platform, runs the pinned Node and
// copies /opt/adc/lib out of a created container, which it removes (M1,
// L6; Mythos M1). Node's start is run only in the native platform's image
// (the fake uname's), and the other's skip printed (#226 entry 141). A dirty tree, or a dist's adc of another commit or a
// dirty one, is refused before podman runs; a NAME podman already holds
// is refused before the build; a list without exactly one image per
// platform, a Node that does not start or reports another version, or an
// image whose /opt/adc/lib is not the label's adapter (or cannot be
// copied, the container still removed), after it. A run that hangs is cut
// by PREPARE_GO_IMAGE_TIMEOUT, its container removed, and the build fails.
func TestPrepareBuild(t *testing.T) {
	f := newBuildFixture(t)
	calls, out, err := f.build(t)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var adapter []string
	for _, arch := range []string{"amd64", "arm64"} {
		d, err := sandbox.AdapterDigest(filepath.Join(f.checkout, "dist/linux-"+arch, "lib/adc"))
		if err != nil {
			t.Fatal(err)
		}
		adapter = append(adapter, `"linux/`+arch+`":"`+d+`"`)
	}
	label := `,"adapter":{` + strings.Join(adapter, ",") + "}}"
	want := []string{"manifest exists localhost/adc-test", "image exists localhost/adc-test", "build", "manifest inspect localhost/adc-test"}
	for _, arch := range []string{"amd64", "arm64"} {
		if arch == "arm64" { // the native platform (the fake uname's aarch64): only its Node is run
			want = append(want, "run --rm --name=adc-prepare-")
		}
		want = append(want, "create --pull=never --network=none --platform=linux/"+arch+" localhost/adc-test true",
			"cp c-"+arch+":/opt/adc/lib ", "rm c-"+arch)
	}
	if len(calls) != len(want) {
		t.Fatalf("podman calls %q", calls)
	}
	for i, c := range calls {
		if !strings.HasPrefix(c, want[i]) {
			t.Errorf("podman call %d: %q, want %q…", i, c, want[i])
		}
	}
	if run := calls[7]; !strings.HasSuffix(run, "-arm64 --pull=never --network=none --platform=linux/arm64 localhost/adc-test /opt/adc/lib/node/bin/node -p process.version + \" \" + process.arch") {
		t.Errorf("the native Node run: %q", run)
	}
	if !strings.Contains(out, "Node's start in localhost/adc-test for linux/amd64 is not checked: the platform is emulated on this linux/arm64 host") ||
		strings.Contains(out, "for linux/arm64 is not checked") {
		t.Errorf("the emulated platform's skip is not printed, or the native one's is: %s", out)
	}
	// On an amd64 host, amd64's Node is the one run.
	g := newBuildFixture(t)
	if err := os.WriteFile(filepath.Join(g.state, "machine"), []byte("x86_64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.version(t, "adc 1234567 (commit "+g.head+", schema set none, go1.27.1, linux/amd64)")
	if calls, out, err := g.build(t); err != nil || len(calls) != 11 || !strings.HasPrefix(calls[4], "run --rm --name=adc-prepare-") ||
		!strings.Contains(calls[4], "--platform=linux/amd64 ") || !strings.Contains(out, "for linux/arm64 is not checked") {
		t.Errorf("on an amd64 host: %v, podman calls %q; %s", err, calls, out)
	}
	if !strings.Contains(calls[2], "--platform=linux/amd64,linux/arm64 --manifest=localhost/adc-test --label=dev.adc.prepared={") ||
		!strings.Contains(calls[2], label+" -f ") {
		t.Errorf("the build %q does not carry the label's adapter %s", calls[2], label)
	}

	before := map[string]func(f buildFixture) error{
		"a dirty tree": func(f buildFixture) error {
			return os.WriteFile(filepath.Join(f.checkout, "new.txt"), nil, 0o644)
		},
		"another commit's adc": func(f buildFixture) error {
			f.version(t, "adc 1234567 (commit "+strings.Repeat("0", 40)+", schema set none, go1.27.1, linux/arm64)")
			return nil
		},
		"a dirty adc": func(f buildFixture) error {
			f.version(t, "adc 1234567-dirty (commit "+f.head+", schema set none, go1.27.1, linux/arm64)")
			return nil
		},
	}
	after := map[string]struct {
		edit  func(f buildFixture) error
		calls int // podman calls before the refusal
	}{
		"an existing list": {func(f buildFixture) error {
			return os.WriteFile(filepath.Join(f.state, "exists-manifest"), nil, 0o644)
		}, 1},
		"an existing image": {func(f buildFixture) error { return os.WriteFile(filepath.Join(f.state, "exists-image"), nil, 0o644) }, 2},
		"the old images still listed": {func(f buildFixture) error {
			return editFile(filepath.Join(f.state, "inspect.json"), func(s string) string {
				return strings.Replace(s, `[`, `[{"platform":{"architecture":"amd64","os":"linux"}},`, 1)
			})
		}, 4},
		"a platform missing": {func(f buildFixture) error {
			return os.WriteFile(filepath.Join(f.state, "inspect.json"), []byte(`{"manifests":[{"platform":{"architecture":"arm64","os":"linux"}}]}`), 0o644)
		}, 4},
		// amd64's create, cp and rm (5-7), then arm64's run (8) and, when it fails, the removal of its container (9).
		"Node does not start": {func(f buildFixture) error { return os.WriteFile(filepath.Join(f.state, "nonode"), nil, 0o644) }, 9},
		"another Node version": {func(f buildFixture) error {
			return os.WriteFile(filepath.Join(f.state, "node"), []byte("v21.0.0\n"), 0o644)
		}, 8},
		// #226 entry 141: a run that hangs is cut, its container removed.
		"a run that hangs": {func(f buildFixture) error {
			return errors.Join(os.WriteFile(filepath.Join(f.state, "hang"), nil, 0o644), os.WriteFile(filepath.Join(f.state, "timeout"), []byte("1\n"), 0o644))
		}, 9},
		// Mythos #373 M1: the image's /opt/adc/lib must have the label's digest.
		"an image whose adapter differs": {func(f buildFixture) error { return os.WriteFile(filepath.Join(f.state, "tamper"), nil, 0o644) }, 7},
		"the other platform's adapter":   {func(f buildFixture) error { return os.WriteFile(filepath.Join(f.state, "swap"), nil, 0o644) }, 7},
		"a copy that fails":              {func(f buildFixture) error { return os.WriteFile(filepath.Join(f.state, "nocp"), nil, 0o644) }, 7},
	}
	for name, edit := range before {
		after[name] = struct {
			edit  func(f buildFixture) error
			calls int
		}{edit, 0}
	}
	for name, c := range after {
		f := newBuildFixture(t)
		if err := c.edit(f); err != nil {
			t.Fatal(err)
		}
		calls, out, err := f.build(t)
		if err == nil || len(calls) != c.calls || !strings.Contains(out, "prepare-go-image: ") {
			t.Errorf("%s: %v, podman calls %q; %s", name, err, calls, out)
		}
		if name == "a run that hangs" && (!strings.Contains(out, "prepare-go-image: podman run did not finish within 1s") ||
			!strings.HasPrefix(calls[len(calls)-1], "rm --force --ignore --time=0 adc-prepare-") || !strings.HasSuffix(calls[len(calls)-1], "-arm64")) {
			t.Errorf("%s: not cut, or its container not removed: podman calls %q; %s", name, calls, out)
		}
	}
}
