// SPDX-License-Identifier: Apache-2.0

package installcheck

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func mustRun(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v %s", name, args, err, out)
	}
}

// The tests' Node release: a version Node never published, so nothing real
// can be fetched for it.
const (
	testVersion = "v22.999.0"
	testNode    = "#!/bin/sh\necho v22.999.0\n"
)

func tarName(arch string) string { return "node-" + testVersion + "-linux-" + arch + ".tar.xz" }

// tarball returns a node tarball for arch holding bin/node and LICENSE;
// edit, when set, changes its tree before it is packed, and appends members
// after (a second tar -r pass, before compression).
func tarball(t *testing.T, arch string, edit func(top string), appendMembers func(tarFile string)) []byte {
	t.Helper()
	src := t.TempDir()
	top := filepath.Join(src, strings.TrimSuffix(tarName(arch), ".tar.xz"))
	write(t, filepath.Join(top, "bin/node"), testNode, 0o755)
	write(t, filepath.Join(top, "LICENSE"), "the licence\n", 0o644)
	if edit != nil {
		edit(top)
	}
	tarFile := filepath.Join(t.TempDir(), "node.tar")
	mustRun(t, src, "tar", "-cf", tarFile, filepath.Base(top))
	if appendMembers != nil {
		appendMembers(tarFile)
	}
	mustRun(t, src, "xz", "-f", tarFile)
	b, err := os.ReadFile(tarFile + ".xz")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// nodeEnv is one dist-node run: a cache, a curl that never reaches the
// network (it records its argv and serves body, or fails), and pins.
type nodeEnv struct {
	dir, cache, shims, pins, out, argv, body string
	env                                      []string
}

func newNodeEnv(t *testing.T) *nodeEnv {
	t.Helper()
	e := &nodeEnv{dir: t.TempDir()}
	e.cache, e.shims = filepath.Join(e.dir, "cache"), filepath.Join(e.dir, "shims")
	e.pins, e.out, e.argv, e.body = filepath.Join(e.dir, "pins"), filepath.Join(e.dir, "out", "node"), filepath.Join(e.dir, "argv"), filepath.Join(e.dir, "body")
	if err := os.Mkdir(e.cache, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(e.shims, "curl"), `#!/bin/sh
printf '%s\n' "$@" >"$ADC_TEST_ARGV"
[ -f "$ADC_TEST_BODY" ] || exit 22
while [ $# -gt 0 ]; do [ "$1" = "-o" ] && out="$2"; shift; done
cat "$ADC_TEST_BODY" >"$out"
`, 0o755)
	e.env = append(testguard.HomeEnv(t), "PATH="+e.shims+":/usr/bin:/bin", "ADC_DIST_CACHE="+e.cache, "ADC_TEST_ARGV="+e.argv, "ADC_TEST_BODY="+e.body)
	return e
}

// pin writes the amd64 pin line for tar.
func (e *nodeEnv) pin(t *testing.T, line string) { write(t, e.pins, line+"\n", 0o644) }

func (e *nodeEnv) goodPin(t *testing.T, tar []byte) {
	e.pin(t, fmt.Sprintf("linux/amd64 %s %s %s %s", testVersion, tarName("x64"), sum(tar), sum([]byte(testNode))))
}

func (e *nodeEnv) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"../../scripts/dist-node.sh", "--pins", e.pins}, args...)...)
	cmd.Env = e.env
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func (e *nodeEnv) packaged(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(e.out)
	return err == nil
}

// dist-node packages bin/node and LICENSE only, from a tarball whose
// SHA-256 and whose bin/node's match their pins; every refusal packages
// nothing.
func TestDistNode(t *testing.T) {
	good := tarball(t, "x64", nil, nil)
	cached := func(t *testing.T, e *nodeEnv, b []byte) {
		write(t, filepath.Join(e.cache, tarName("x64")), string(b), 0o600)
	}
	for name, tc := range map[string]struct {
		setup func(t *testing.T, e *nodeEnv)
		want  string // "" is success
		check func(t *testing.T, e *nodeEnv)
	}{
		"cached": {func(t *testing.T, e *nodeEnv) { cached(t, e, good); e.goodPin(t, good) }, "", func(t *testing.T, e *nodeEnv) {
			if _, err := os.Stat(e.argv); err == nil {
				t.Error("downloaded with a good cache")
			}
		}},
		"downloaded": {func(t *testing.T, e *nodeEnv) { write(t, e.body, string(good), 0o600); e.goodPin(t, good) }, "", func(t *testing.T, e *nodeEnv) {
			argv, _ := os.ReadFile(e.argv)
			for _, want := range []string{"--fail\n", "--proto\n=https\n", "--proto-redir\n=https\n", "--tlsv1.2\n",
				"\nhttps://nodejs.org/dist/" + testVersion + "/" + tarName("x64") + "\n"} {
				if !strings.Contains(string(argv), want) {
					t.Errorf("curl argv lacks %q:\n%s", want, argv)
				}
			}
			if entries, _ := os.ReadDir(e.cache); len(entries) != 1 || entries[0].Name() != tarName("x64") {
				t.Errorf("the cache holds %v", entries)
			}
		}},
		"a new cache is private": {func(t *testing.T, e *nodeEnv) {
			cached(t, e, good) // in the old cache, which is not used
			e.goodPin(t, good)
			write(t, e.body, string(good), 0o600)
			e.env = append(e.env, "ADC_DIST_CACHE="+filepath.Join(e.dir, "new-cache"))
		}, "", func(t *testing.T, e *nodeEnv) {
			if fi, err := os.Stat(filepath.Join(e.dir, "new-cache")); err != nil || fi.Mode().Perm() != 0o700 {
				t.Errorf("the new cache: %v %v", fi, err)
			}
		}},
		"a corrupted cache is replaced": {func(t *testing.T, e *nodeEnv) {
			cached(t, e, []byte("not the pinned bytes"))
			write(t, e.body, string(good), 0o600)
			e.goodPin(t, good)
		}, "", nil},
		"a corrupted cache, no download": {func(t *testing.T, e *nodeEnv) { cached(t, e, []byte("not the pinned bytes")); e.goodPin(t, good) },
			"downloading " + tarName("x64") + " failed", func(t *testing.T, e *nodeEnv) {
				if _, err := os.Lstat(filepath.Join(e.cache, tarName("x64"))); err == nil {
					t.Error("the corrupted cache entry was kept")
				}
			}},
		"another bin/node": {func(t *testing.T, e *nodeEnv) {
			cached(t, e, good)
			e.pin(t, fmt.Sprintf("linux/amd64 %s %s %s %s", testVersion, tarName("x64"), sum(good), strings.Repeat("1", 64)))
		}, "bin/node in " + tarName("x64") + " does not match", nil},
		"a malformed version": {func(t *testing.T, e *nodeEnv) {
			e.pin(t, fmt.Sprintf("linux/amd64 22.999.0 node-22.999.0-linux-x64.tar.xz %s %s", sum(good), sum([]byte(testNode))))
		}, "the pinned version for linux/amd64 is malformed", nil},
		"another platform's tarball": {func(t *testing.T, e *nodeEnv) {
			e.pin(t, fmt.Sprintf("linux/amd64 %s %s %s %s", testVersion, tarName("arm64"), sum(good), sum([]byte(testNode))))
		}, "is not " + tarName("x64"), nil},
		"another version's tarball": {func(t *testing.T, e *nodeEnv) {
			e.pin(t, fmt.Sprintf("linux/amd64 %s node-v22.1.0-linux-x64.tar.xz %s %s", testVersion, sum(good), sum([]byte(testNode))))
		}, "is not " + tarName("x64"), nil},
		"a malformed tarball sum": {func(t *testing.T, e *nodeEnv) {
			e.pin(t, fmt.Sprintf("linux/amd64 %s %s abc %s", testVersion, tarName("x64"), sum([]byte(testNode))))
		}, "the pinned tarball SHA-256 for linux/amd64 is malformed", nil},
		"a malformed bin/node sum": {func(t *testing.T, e *nodeEnv) {
			e.pin(t, fmt.Sprintf("linux/amd64 %s %s %s abc", testVersion, tarName("x64"), sum(good)))
		}, "the pinned bin/node SHA-256 for linux/amd64 is malformed", nil},
		"a LICENSE symlink": {func(t *testing.T, e *nodeEnv) {
			b := tarball(t, "x64", func(top string) {
				rm(t, filepath.Join(top, "LICENSE"))
				_ = os.Symlink("/etc/hostname", filepath.Join(top, "LICENSE"))
			}, nil)
			cached(t, e, b)
			e.goodPin(t, b)
		}, "LICENSE in " + tarName("x64") + " is not a regular file", nil},
		"bin/node a symlink after a regular member": {func(t *testing.T, e *nodeEnv) {
			same := filepath.Join(e.dir, "same-bytes")
			write(t, same, testNode, 0o755) // the pinned bytes: only the link check refuses it
			b := tarball(t, "x64", nil, func(tarFile string) {
				alt := t.TempDir()
				top := strings.TrimSuffix(tarName("x64"), ".tar.xz")
				if err := os.MkdirAll(filepath.Join(alt, top, "bin"), 0o755); err != nil {
					t.Fatal(err)
				}
				_ = os.Symlink(same, filepath.Join(alt, top, "bin/node"))
				mustRun(t, alt, "tar", "-rf", tarFile, top+"/bin/node")
			})
			cached(t, e, b)
			e.goodPin(t, b)
		}, "bin/node in " + tarName("x64") + " is not a regular file", nil},
		"the private copy is checked": {func(t *testing.T, e *nodeEnv) {
			cached(t, e, good)
			e.goodPin(t, good)
			write(t, filepath.Join(e.shims, "cp"), "#!/bin/sh\neval \"last=\\${$#}\"\necho swapped >\"$last\"\n", 0o755)
		}, tarName("x64") + " does not match its pinned SHA-256", nil},
		"a planted .part symlink": {func(t *testing.T, e *nodeEnv) {
			write(t, filepath.Join(e.dir, "victim"), "victim", 0o600)
			_ = os.Symlink(filepath.Join(e.dir, "victim"), filepath.Join(e.cache, tarName("x64")+".part"))
			write(t, e.body, string(good), 0o600)
			e.goodPin(t, good)
		}, "", func(t *testing.T, e *nodeEnv) {
			if b, _ := os.ReadFile(filepath.Join(e.dir, "victim")); string(b) != "victim" {
				t.Error("the download wrote through a planted link")
			}
		}},
		"a symlinked cache entry": {func(t *testing.T, e *nodeEnv) {
			write(t, filepath.Join(e.dir, "elsewhere"), string(good), 0o600)
			_ = os.Symlink(filepath.Join(e.dir, "elsewhere"), filepath.Join(e.cache, tarName("x64")))
			e.goodPin(t, good)
		}, "the cached " + tarName("x64") + " is a symlink", nil},
		"a world-writable cache": {func(t *testing.T, e *nodeEnv) {
			cached(t, e, good)
			e.goodPin(t, good)
			_ = os.Chmod(e.cache, 0o777)
		}, "is group- or world-writable", nil},
		"a group-writable cache": {func(t *testing.T, e *nodeEnv) {
			cached(t, e, good)
			e.goodPin(t, good)
			_ = os.Chmod(e.cache, 0o770)
		}, "is group- or world-writable", nil},
		"a symlinked cache": {func(t *testing.T, e *nodeEnv) {
			e.goodPin(t, good)
			_ = os.Symlink(e.cache, filepath.Join(e.dir, "link"))
			e.env = append(e.env, "ADC_DIST_CACHE="+filepath.Join(e.dir, "link"))
		}, "is not a directory of this user's", nil},
		"a cache another user owns": {func(t *testing.T, e *nodeEnv) {
			if os.Geteuid() == 0 {
				t.Skip("root owns /usr")
			}
			e.goodPin(t, good)
			e.env = append(e.env, "ADC_DIST_CACHE=/usr")
		}, "the cache /usr is not a directory of this user's", nil},
		"no cache directory": {func(t *testing.T, e *nodeEnv) {
			e.goodPin(t, good)
			e.env = []string{"PATH=" + e.shims + ":/usr/bin:/bin"}
		}, "no cache directory: set ADC_DIST_CACHE, XDG_CACHE_HOME or HOME", nil},
	} {
		t.Run(name, func(t *testing.T) {
			e := newNodeEnv(t)
			tc.setup(t, e)
			text, err := e.run(t, "linux/amd64", e.out)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("%v: %s", err, text)
			case tc.want != "" && (err == nil || !strings.Contains(text, tc.want)):
				t.Fatalf("want %q: %v: %s", tc.want, err, text)
			case tc.want != "" && e.packaged(t):
				t.Fatal("something was packaged")
			case tc.want == "":
				got, _ := os.ReadFile(filepath.Join(e.out, "bin/node"))
				entries, _ := os.ReadDir(e.out)
				if !bytes.Equal(got, []byte(testNode)) || len(entries) != 2 || entries[0].Name() != "LICENSE" || entries[1].Name() != "bin" {
					t.Fatalf("packaged %v, bin/node %q", entries, got)
				}
			}
			if tc.check != nil {
				tc.check(t, e)
			}
		})
	}
}

// --verify checks a packaged bin/node against its pin, by sum alone.
func TestDistNodeVerify(t *testing.T) {
	e := newNodeEnv(t)
	e.goodPin(t, []byte("tarball"))
	write(t, filepath.Join(e.out, "bin/node"), testNode, 0o755)
	if text, err := e.run(t, "--verify", "linux/amd64", e.out); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	write(t, filepath.Join(e.out, "bin/node"), testNode+"# tampered\n", 0o755)
	if text, err := e.run(t, "--verify", "linux/amd64", e.out); err == nil || !strings.Contains(text, "does not match the pin for linux/amd64") {
		t.Fatalf("a tampered node: %v: %s", err, text)
	}
}

// The pins in scripts/node-pins are well formed, for exactly the server
// platforms, one Node 22 version.
func TestNodePins(t *testing.T) {
	b, err := os.ReadFile("../../scripts/node-pins")
	if err != nil {
		t.Fatal(err)
	}
	var platforms []string
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		arch := map[string]string{"linux/amd64": "x64", "linux/arm64": "arm64"}[f[0]]
		if len(f) != 5 || arch == "" || !strings.HasPrefix(f[1], "v22.") || f[2] != "node-"+f[1]+"-linux-"+arch+".tar.xz" ||
			len(f[3]) != 64 || len(f[4]) != 64 {
			t.Errorf("a malformed pin: %q", line)
		}
		platforms = append(platforms, f[0])
	}
	if !slices.Equal(platforms, []string{"linux/amd64", "linux/arm64"}) {
		t.Errorf("pinned platforms %v", platforms)
	}
}

// fakeRepo is a git checkout with the paths dist-adapters reads, one
// third-party package (a, with its licence), untracked secrets beside the
// tracked files, and a pnpm on PATH that lists listing (ROOT: the
// repository) and whose store status exits storeRC.
func fakeRepo(t *testing.T, listing string, scripts ...string) (root string, env []string) {
	t.Helper()
	root = t.TempDir()
	for _, s := range append([]string{"dist-adapters.sh"}, scripts...) {
		b, err := os.ReadFile("../../scripts/" + s)
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(root, "scripts", s), string(b), 0o755)
	}
	for name, content := range map[string]string{
		"LICENSE": "apache", "NOTICE": "notice", "adapters/fake/package.json": "{}", "adapters/fake/src/main.ts": "",
		"adapters/fake/test/fake.test.ts": "", "adapters/contract/package.json": "{}", "adapters/contract/src/index.ts": "",
		"gen/ts/package.json": "{}", "gen/ts/validators.cjs": "", "gen/ts/formats.cjs": "", "gen/ts/types/x.ts": "",
		"fixtures/t0/py-mini/tasks/py-mini-feature-1/task.json": "{}", "fixtures/t0/py-mini/repo/m.py": "",
	} {
		write(t, filepath.Join(root, name), content, 0o644)
	}
	env = append(testguard.HomeEnv(t), "GIT_CONFIG_NOSYSTEM=1")
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = root, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", "--", ".")
	// Untracked, after the index is written: never packaged.
	for _, name := range []string{"adapters/fake/src/.env", "gen/ts/.env", "fixtures/t0/py-mini/.venv/bin/python", "fixtures/t0/py-mini/repo/__pycache__/m.pyc"} {
		write(t, filepath.Join(root, name), "SECRET", 0o644)
	}
	pkg := "adapters/node_modules/.pnpm/a@1.0.0/node_modules/a/"
	write(t, filepath.Join(root, pkg, "index.js"), "", 0o644)
	write(t, filepath.Join(root, pkg, "LICENSE"), "MIT", 0o644)
	write(t, filepath.Join(root, pkg, "package.json"), `{"name":"a","license":"MIT"}`, 0o644)
	bin := t.TempDir()
	write(t, filepath.Join(bin, "pnpm"), "#!/bin/sh\n[ \"$1\" = store ] && exit \"${ADC_TEST_STORE_RC:-0}\"\ncat \"$ADC_TEST_LISTING\"\n", 0o755)
	listFile := filepath.Join(t.TempDir(), "listing")
	write(t, listFile, strings.ReplaceAll(listing, "ROOT", root), 0o644)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	return root, append(env, "PATH="+bin+":"+filepath.Dir(node)+":/usr/bin:/bin", "ADC_TEST_LISTING="+listFile)
}

const goodListing = "ROOT/adapters/fake\nROOT/adapters/contract\nROOT/gen/ts\nROOT/adapters/node_modules/.pnpm/a@1.0.0/node_modules/a\n"

// files lists the regular files and symlinks under dir.
func files(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// dist-adapters packages exactly the tracked runtime files, the licences
// and an allowed closure, and refuses anything else.
func TestDistAdaptersRefusals(t *testing.T) {
	pkg := "ROOT/adapters/node_modules/.pnpm/a@1.0.0/node_modules/a"
	inPkg := func(root, name string) string { return filepath.Join(strings.ReplaceAll(pkg, "ROOT", root), name) }
	for name, tc := range map[string]struct {
		listing string
		change  func(root string, env *[]string)
		want    string
	}{
		"good":            {goodListing, nil, ""},
		"no licence file": {goodListing, func(root string, _ *[]string) { rm(t, inPkg(root, "LICENSE")) }, "a carries no licence file"},
		"a licence only in a subdirectory": {goodListing, func(root string, _ *[]string) {
			rm(t, inPkg(root, "LICENSE"))
			write(t, inPkg(root, "docs/LICENSE"), "MIT", 0o644)
		}, "a carries no licence file"},
		"a licence off the allowlist": {goodListing, func(root string, _ *[]string) {
			write(t, inPkg(root, "package.json"), `{"name":"a","license":"GPL-3.0-only"}`, 0o644)
		}, "declares the licence GPL-3.0-only, which is not on the allowlist"},
		"no licence declared": {goodListing, func(root string, _ *[]string) {
			write(t, inPkg(root, "package.json"), `{"name":"a"}`, 0o644)
		}, "declares the licence undefined"},
		"a modified store":   {goodListing, func(_ string, env *[]string) { *env = append(*env, "ADC_TEST_STORE_RC=1") }, "pnpm store status failed"},
		"not a git checkout": {goodListing, func(root string, _ *[]string) { rm(t, filepath.Join(root, ".git")) }, "is not a git checkout"},
		"outside the store":  {goodListing + "/usr/lib/node_modules/b\n", nil, "an unexpected production dependency"},
		"another workspace":  {goodListing + "ROOT/adapters/conformance\n", nil, "an unexpected production dependency"},
		"an @adc package":    {goodListing + "ROOT/adapters/node_modules/.pnpm/x/node_modules/@adc/evil\n", nil, "an unexpected package name"},
		"a dot name":         {goodListing + "ROOT/adapters/node_modules/.pnpm/x/node_modules/.\n", nil, "an unexpected package name: ."},
		"a dot-dot name":     {goodListing + "ROOT/adapters/node_modules/.pnpm/x/node_modules/..\n", nil, "an unexpected package name: .."},
		"a spaced name":      {goodListing + "ROOT/adapters/node_modules/.pnpm/x/node_modules/b c\n", nil, "an unexpected package name"},
		"listed twice":       {goodListing + pkg + "\n", nil, "a is listed twice"},
		"no third party":     {"ROOT/adapters/fake\n", nil, "no third-party dependency"},
		"no py-mini task": {goodListing, func(root string, env *[]string) {
			cmd := exec.Command("git", "rm", "-rq", "--cached", "--", "fixtures/t0/py-mini/tasks")
			cmd.Dir, cmd.Env = root, *env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v %s", err, out)
			}
		}, "py-mini-feature-1 is missing"},
		"no NOTICE": {goodListing, func(root string, env *[]string) {
			cmd := exec.Command("git", "rm", "-q", "--cached", "--", "NOTICE")
			cmd.Dir, cmd.Env = root, *env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v %s", err, out)
			}
		}, "NOTICE is missing"},
		"a symlink in a dep": {goodListing, func(root string, _ *[]string) { _ = os.Symlink("/etc/passwd", inPkg(root, "p")) }, "unexpected symlinks"},
	} {
		t.Run(name, func(t *testing.T) {
			root, env := fakeRepo(t, tc.listing)
			if tc.change != nil {
				tc.change(root, &env)
			}
			out := filepath.Join(t.TempDir(), "lib", "adc")
			cmd := exec.Command("bash", filepath.Join(root, "scripts/dist-adapters.sh"), out)
			cmd.Env = env
			text, err := cmd.CombinedOutput()
			if tc.want != "" {
				if err == nil || !strings.Contains(string(text), tc.want) {
					t.Fatalf("want %q: %v: %s", tc.want, err, text)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v: %s", err, text)
			}
			want := []string{"LICENSE", "NOTICE", "adapters/contract/package.json", "adapters/contract/src/index.ts", "adapters/fake/package.json",
				"adapters/fake/src/main.ts", "fixtures/t0/py-mini/repo/m.py", "fixtures/t0/py-mini/tasks/py-mini-feature-1/task.json",
				"gen/ts/formats.cjs", "gen/ts/package.json", "gen/ts/validators.cjs", "node_modules/@adc/contract", "node_modules/@adc/contract-gen",
				"node_modules/a/LICENSE", "node_modules/a/index.js", "node_modules/a/package.json"}
			if got := files(out); !slices.Equal(got, want) {
				t.Fatalf("packaged %v", got)
			}
		})
	}
}

// dist-lib fails when any platform fails: a wrong amd64 pin fails it even
// though arm64, packaged last, succeeds. A good run leaves, per platform,
// a verified node, normalised modes and a SHA256SUMS that checks.
func TestDistLib(t *testing.T) {
	cache := t.TempDir()
	if err := os.Chmod(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	pinLines := ""
	for _, arch := range []string{"x64", "arm64"} {
		b := tarball(t, arch, nil, nil)
		write(t, filepath.Join(cache, tarName(arch)), string(b), 0o600)
		platform := map[string]string{"x64": "linux/amd64", "arm64": "linux/arm64"}[arch]
		pinLines += fmt.Sprintf("%s %s %s %s %s\n", platform, testVersion, tarName(arch), sum(b), sum([]byte(testNode)))
	}
	for name, tc := range map[string]struct {
		pins string
		want string
	}{
		"good":              {pinLines, ""},
		"a wrong amd64 pin": {strings.Replace(pinLines, sum([]byte(testNode)), strings.Repeat("1", 64), 1), "packaging node for linux/amd64 failed"},
	} {
		t.Run(name, func(t *testing.T) {
			root, env := fakeRepo(t, goodListing, "dist-node.sh", "dist-lib.sh")
			pins := filepath.Join(t.TempDir(), "pins")
			write(t, pins, tc.pins, 0o644)
			if err := os.Chmod(filepath.Join(root, "fixtures/t0/py-mini/repo/m.py"), 0o600); err != nil {
				t.Fatal(err)
			}
			dist := filepath.Join(t.TempDir(), "dist")
			cmd := exec.Command("bash", filepath.Join(root, "scripts/dist-lib.sh"), "--pins", pins, dist, "linux/amd64", "linux/arm64")
			cmd.Env = append(env, "ADC_DIST_CACHE="+cache)
			text, err := cmd.CombinedOutput()
			if tc.want != "" {
				if err == nil || !strings.Contains(string(text), tc.want) {
					t.Fatalf("want %q: %v: %s", tc.want, err, text)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v: %s", err, text)
			}
			for _, d := range []string{"linux-amd64", "linux-arm64"} {
				lib := filepath.Join(dist, d, "lib/adc")
				check := exec.Command("sha256sum", "--check", "--strict", "--quiet", "SHA256SUMS")
				check.Dir = lib
				if out, err := check.CombinedOutput(); err != nil {
					t.Fatalf("%s: %v %s", d, err, out)
				}
				sums, _ := os.ReadFile(filepath.Join(lib, "SHA256SUMS"))
				if n := strings.Count(string(sums), "\n"); n != len(files(lib))-3 { // not SHA256SUMS or the two links
					t.Errorf("%s: SHA256SUMS lists %d files", d, n)
				}
				if fi, err := os.Stat(filepath.Join(lib, "fixtures/t0/py-mini/repo/m.py")); err != nil || fi.Mode().Perm() != 0o644 {
					t.Errorf("%s: mode not normalised: %v %v", d, fi.Mode(), err)
				}
			}
		})
	}
}

// dist-adapters on the real workspace: the packaged fake answers describe
// with the host's node and no source tree on its resolution path, and the
// third-party set is exactly the fake's closure. It needs the installed
// workspace and pnpm, which the test sandbox lacks: make ci's install-check
// runs the packaged tree through the pinned node there.
func TestDistAdapters(t *testing.T) {
	if _, err := exec.LookPath("pnpm"); err != nil {
		t.Skip("pnpm is not on PATH (make ci's install-check covers the packaged tree)")
	}
	if _, err := os.Stat("../../adapters/node_modules"); err != nil {
		t.Skip("the adapters workspace is not installed")
	}
	out := filepath.Join(t.TempDir(), "lib", "adc")
	if text, err := exec.Command("bash", "../../scripts/dist-adapters.sh", out).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	entries, err := os.ReadDir(filepath.Join(out, "node_modules"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{"@adc", "ajv", "fast-deep-equal", "fast-uri", "json-schema-traverse", "require-from-string"}; !slices.Equal(names, want) {
		t.Errorf("third-party packages %v, want %v", names, want)
	}
	for _, f := range []string{"LICENSE", "NOTICE", "adapters/fake/src/main.ts", "adapters/contract/src/index.ts", "gen/ts/validators.cjs",
		"node_modules/require-from-string/license", "fixtures/t0/py-mini/tasks/py-mini-feature-1/task.json"} {
		if fi, err := os.Lstat(filepath.Join(out, f)); err != nil || !fi.Mode().IsRegular() {
			t.Errorf("%s: %v", f, err)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	cmd := exec.Command(node, filepath.Join(out, "adapters/fake/src/main.ts"))
	cmd.Dir, cmd.Env = t.TempDir(), []string{"PATH=/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"describe"}` + "\n")
	if got, err := cmd.Output(); err != nil || !strings.Contains(string(got), `"harness":"fake"`) {
		t.Fatalf("the packaged fake: %v %s", err, got)
	}
	if text, err := exec.Command("bash", "../../scripts/dist-adapters.sh", out).CombinedOutput(); err == nil || !strings.Contains(string(text), "already holds adapters") {
		t.Errorf("a second packaging into the same directory: %v %s", err, text)
	}
}
