// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
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
	"syscall"
	"testing"
	"time"

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
// each call in STATE/log and:
//   - hangs (sleep 60) on a call whose words start with STATE/hang's line,
//     ignoring SIGTERM when STATE/ignoreterm exists;
//   - answers info with STATE/native (linux/arm64), exists from
//     STATE/exists-KIND, inspect from STATE/inspect.json, and manifest rm
//     (failing when STATE/normrm exists);
//   - runs Node with STATE/node and the platform's arch (failing when
//     STATE/nonode exists);
//   - copies NAME-ARCH:… with the dist's lib/adc of ARCH (failing when
//     STATE/nocp exists; the other ARCH's when STATE/swap does; with a
//     byte appended to http-chat's main.ts when STATE/tamper does).
//
// A fake uname -m answers STATE/machine (aarch64): the dist's adc is
// chosen by it, the native platform by podman info.
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
if [ -e "$state/hang" ]; then
  case "$*" in
  "$(cat "$state/hang")"*)
    [ -e "$state/ignoreterm" ] && trap '' TERM
    exec sleep 60 ;;
  esac
fi
case "$1 $2" in
"manifest exists" | "image exists") [ -e "$state/exists-$1" ] && exit 0; exit 1 ;;
"manifest inspect") exec cat "$state/inspect.json" ;;
"manifest rm") [ -e "$state/normrm" ] && exit 1; exit 0 ;;
esac
for a in "$@"; do
  case "$a" in --platform=linux/amd64) arch=amd64 ;; --platform=linux/arm64) arch=arm64 ;; esac
done
case "$1" in
info) exec cat "$state/native" ;;
build | rm | create) exit 0 ;;
run)
  [ -e "$state/nonode" ] && exit 1
  [ "$arch" = amd64 ] && arch=x64
  echo "$(cat "$state/node") $arch" ;;
cp)
  [ -e "$state/nocp" ] && exit 1
  arch="${2%%:*}" arch="${arch##*-}"
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
		os.WriteFile(filepath.Join(state, "native"), []byte("linux/arm64\n"), 0o644),
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
// the podman calls. The script runs in a process group of its own, which a
// safety net kills whole if it is still running after buildNet (a mutant
// that drops a bound: Opus #390 L1), so nothing it started outlives the
// row.
func (f buildFixture) build(t *testing.T) ([]string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), buildNet)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(f.checkout, "scripts/prepare-go-image.sh"), "build", "go-mini", "localhost/adc-test")
	cmd.Env = f.env
	if b, err := os.ReadFile(filepath.Join(f.state, "timeout")); err == nil {
		cmd.Env = append(slices.Clone(f.env), "PREPARE_GO_IMAGE_TIMEOUT="+strings.TrimSpace(string(b)))
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	log, _ := os.ReadFile(filepath.Join(f.state, "log"))
	return strings.FieldsFunc(string(log), func(r rune) bool { return r == '\n' }), string(out), err
}

// buildNet is build's safety net; a row never comes near it.
const buildNet = 2 * time.Minute

// #373 L5: build's gates, against a fake podman. A clean checkout whose
// dist's adc is HEAD's reads podman's native platform, then builds NAME
// with the label of the dist's adapters, lists its images and, per
// platform, copies /opt/adc/lib out of a named container, which it
// removes (M1, L6; Mythos M1; Opus #390 L2); Node's start is run only in
// the native platform's image, and the other's skip printed (#226 entry
// 141; Opus #390 L3).
//
// Refused before podman runs: a dirty tree, a dist's adc of another commit
// or a dirty one, a PREPARE_GO_IMAGE_TIMEOUT that is no number. Refused
// before the build: PLATFORMS without the native platform, a NAME podman
// already holds. Refused after it, NAME then removed (Opus #390 M1): a list
// without exactly one image per platform, a Node that does not start or
// reports another version, an image whose /opt/adc/lib is not the label's
// adapter or cannot be copied (its container still removed). Every podman
// call but the build is cut by PREPARE_GO_IMAGE_TIMEOUT, killed when it
// ignores the cut (exit 137), and fails the build (Opus #390 M2).
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
	want := []string{"info --format {{.Host.OS}}/{{.Host.Arch}}", "manifest exists localhost/adc-test", "image exists localhost/adc-test", "build",
		"manifest inspect localhost/adc-test"}
	copied := []string{"create --name=adc-prepare-", "cp adc-prepare-", "rm --force --ignore --time=0 adc-prepare-"}
	want = append(append(append(want, copied...), "run --rm --name=adc-prepare-"), copied...)
	if len(calls) != len(want) {
		t.Fatalf("podman calls %q", calls)
	}
	for i, c := range calls {
		if !strings.HasPrefix(c, want[i]) {
			t.Errorf("podman call %d: %q, want %q…", i, c, want[i])
		}
	}
	for i, arch := range map[int]string{5: "amd64", 6: "amd64", 7: "amd64", 9: "arm64", 10: "arm64", 11: "arm64"} {
		if !strings.Contains(calls[i], "-copy-"+arch) {
			t.Errorf("podman call %d: %q is not %s's copy container", i, calls[i], arch)
		}
	}
	if !strings.Contains(calls[5], " --platform=linux/"+"amd64 localhost/adc-test true") {
		t.Errorf("the create: %q", calls[5])
	}
	if run := calls[8]; !strings.HasSuffix(run, "-arm64 --pull=never --network=none --platform=linux/arm64 localhost/adc-test /opt/adc/lib/node/bin/node -p process.version + \" \" + process.arch") {
		t.Errorf("the native Node run: %q", run)
	}
	if !strings.Contains(out, "Node's start in localhost/adc-test for linux/amd64 is not checked: the platform is emulated on this linux/arm64 host") ||
		strings.Contains(out, "for linux/arm64 is not checked") || strings.Contains(out, "do not push") {
		t.Errorf("the emulated platform's skip is not printed, or the native one's is, or NAME is removed: %s", out)
	}
	if !strings.Contains(calls[3], "--platform=linux/amd64,linux/arm64 --manifest=localhost/adc-test --label=dev.adc.prepared={") ||
		!strings.Contains(calls[3], label+" -f ") {
		t.Errorf("the build %q does not carry the label's adapter %s", calls[3], label)
	}
	// Where podman's native platform is amd64, amd64's Node is the one run.
	g := newBuildFixture(t)
	if err := os.WriteFile(filepath.Join(g.state, "native"), []byte("linux/amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if calls, out, err := g.build(t); err != nil || len(calls) != 12 || !strings.HasPrefix(calls[5], "run --rm --name=adc-prepare-") ||
		!strings.Contains(calls[5], "--platform=linux/amd64 ") || !strings.Contains(out, "for linux/arm64 is not checked") {
		t.Errorf("podman native on amd64: %v, podman calls %q; %s", err, calls, out)
	}

	type row struct {
		edit    func(f buildFixture) error
		calls   int    // podman calls before the refusal, NAME's removal included
		removed bool   // NAME removed: the last call
		says    string // in the output
	}
	file := func(name, body string) func(f buildFixture) error {
		return func(f buildFixture) error { return os.WriteFile(filepath.Join(f.state, name), []byte(body), 0o644) }
	}
	// hang cuts the call starting with prefix after 1 s (ignoring TERM: killed 10 s later).
	hang := func(prefix string, ignoreTerm bool) func(f buildFixture) error {
		return func(f buildFixture) error {
			err := errors.Join(file("hang", prefix)(f), file("timeout", "1\n")(f))
			if ignoreTerm {
				err = errors.Join(err, file("ignoreterm", "")(f))
			}
			return err
		}
	}
	const cut = "was cut after 1s or killed (exit 124)"
	rows := map[string]row{
		"a dirty tree": {func(f buildFixture) error { return os.WriteFile(filepath.Join(f.checkout, "new.txt"), nil, 0o644) }, 0, false, "uncommitted changes"},
		"another commit's adc": {func(f buildFixture) error {
			f.version(t, "adc 1234567 (commit "+strings.Repeat("0", 40)+", schema set none, go1.27.1, linux/arm64)")
			return nil
		}, 0, false, "is not built from"},
		"a dirty adc": {func(f buildFixture) error {
			f.version(t, "adc 1234567-dirty (commit "+f.head+", schema set none, go1.27.1, linux/arm64)")
			return nil
		}, 0, false, "is not built from"},
		"a timeout that is no number":           {file("timeout", "abc\n"), 0, false, "PREPARE_GO_IMAGE_TIMEOUT is a number of seconds"},
		"PLATFORMS without the native platform": {file("native", "linux/riscv64\n"), 1, false, "lacks podman's native platform linux/riscv64"},
		"an existing list":                      {file("exists-manifest", ""), 2, false, "already exists"},
		"an existing image":                     {file("exists-image", ""), 3, false, "already exists"},
		"the old images still listed": {func(f buildFixture) error {
			return editFile(filepath.Join(f.state, "inspect.json"), func(s string) string {
				return strings.Replace(s, `[`, `[{"platform":{"architecture":"amd64","os":"linux"}},`, 1)
			})
		}, 6, true, "rather than one image"},
		"a platform missing":   {file("inspect.json", `{"manifests":[{"platform":{"architecture":"arm64","os":"linux"}}]}`), 6, true, "rather than one image"},
		"Node does not start":  {file("nonode", ""), 11, true, "Node does not start"}, // the run (9), its container's removal, NAME's
		"another Node version": {file("node", "v21.0.0\n"), 10, true, "reports 'v21.0.0 arm64'"},
		// Mythos #373 M1: the image's /opt/adc/lib must have the label's digest.
		"an image whose adapter differs": {file("tamper", ""), 9, true, "not the label's"},
		"the other platform's adapter":   {file("swap", ""), 9, true, "not the label's"},
		"a copy that fails":              {file("nocp", ""), 9, true, "or copy /opt/adc/lib out of it"},
		"NAME cannot be removed": {func(f buildFixture) error { return errors.Join(file("nonode", "")(f), file("normrm", "")(f)) }, 11, true,
			"could not be removed: do not push it"},
		// #226 entry 141, Opus #390 M2: every bounded call, cut.
		"info hangs":             {hang("info", false), 1, false, cut},
		"manifest exists hangs":  {hang("manifest exists", false), 2, false, cut},
		"image exists hangs":     {hang("image exists", false), 3, false, cut},
		"manifest inspect hangs": {hang("manifest inspect", false), 6, true, cut},
		"create hangs":           {hang("create", false), 8, true, cut}, // its container still removed (7)
		"cp hangs":               {hang("cp", false), 9, true, cut},
		"rm hangs":               {hang("rm --force", false), 9, true, cut},
		"run hangs":              {hang("run", false), 11, true, cut},
		"run ignores the cut":    {hang("run", true), 11, true, "was cut after 1s or killed (exit 137)"},
	}
	for name, c := range rows {
		f := newBuildFixture(t)
		if err := c.edit(f); err != nil {
			t.Fatal(err)
		}
		calls, out, err := f.build(t)
		removed := len(calls) > 0 && calls[len(calls)-1] == "manifest rm localhost/adc-test"
		if err == nil || len(calls) != c.calls || removed != c.removed || !strings.Contains(out, c.says) ||
			c.removed && name != "NAME cannot be removed" && !strings.Contains(out, "localhost/adc-test failed and is removed: do not push it") {
			t.Errorf("%s: %v, podman calls %q; %s", name, err, calls, out)
		}
		if strings.HasPrefix(name, "run") && !strings.HasPrefix(calls[9], "rm --force --ignore --time=0 adc-prepare-") {
			t.Errorf("%s: the cut run's container is not removed: %q", name, calls)
		}
	}
}
