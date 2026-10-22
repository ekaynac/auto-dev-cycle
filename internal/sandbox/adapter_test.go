// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// closureTree writes a small lib/adc into a new directory: the closure,
// with Node executable and @adc's two relative links, and an adapter
// beside it that the closure leaves out.
func closureTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]os.FileMode{
		"node/bin/node": 0o755, "node/LICENSE": 0o644, "adapters/http-chat/package.json": 0o644,
		"adapters/http-chat/src/main.ts": 0o644, "adapters/contract/src/index.ts": 0o644,
		"gen/ts/validators.cjs": 0o644, "node_modules/ajv/index.js": 0o644, "adapters/fake/src/main.ts": 0o644,
	}
	for name, mode := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(name+"\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules/@adc"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"contract": "../../adapters/contract", "contract-gen": "../../gen/ts"} {
		if err := os.Symlink(target, filepath.Join(dir, "node_modules/@adc", name)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func adapterDigest(t *testing.T, dir string) string {
	t.Helper()
	d, err := AdapterDigest(dir)
	if err != nil {
		t.Fatalf("AdapterDigest: %v", err)
	}
	return d
}

// P22a-5d: two copies of a closure have one digest, sha256:HEX; a changed
// byte, execute bit, added file or retargeted link changes it; another
// mode bit, or a file outside the closure, does not.
func TestAdapterDigest(t *testing.T) {
	base := adapterDigest(t, closureTree(t))
	if !digestPin.MatchString(base) || adapterDigest(t, closureTree(t)) != base {
		t.Fatalf("two copies: %s and %s", base, adapterDigest(t, closureTree(t)))
	}
	cases := []struct {
		name    string
		changes bool
		edit    func(dir string) error
	}{
		{"a changed byte", true, func(d string) error {
			return os.WriteFile(filepath.Join(d, "adapters/http-chat/src/main.ts"), []byte("adapters/http-chat/src/main.tS\n"), 0o644)
		}},
		{"Node's execute bit cleared", true, func(d string) error { return os.Chmod(filepath.Join(d, "node/bin/node"), 0o644) }},
		{"a source made executable", true, func(d string) error { return os.Chmod(filepath.Join(d, "gen/ts/validators.cjs"), 0o744) }},
		{"an added file", true, func(d string) error { return os.WriteFile(filepath.Join(d, "node_modules/ajv/extra.js"), nil, 0o644) }},
		{"an added directory", true, func(d string) error { return os.Mkdir(filepath.Join(d, "gen/ts/more"), 0o755) }},
		{"a retargeted link", true, func(d string) error {
			l := filepath.Join(d, "node_modules/@adc/contract-gen")
			return errors.Join(os.Remove(l), os.Symlink("../../adapters/contract", l))
		}},
		{"a write bit", false, func(d string) error { return os.Chmod(filepath.Join(d, "node_modules/ajv/index.js"), 0o600) }},
		{"a file outside the closure", false, func(d string) error {
			return errors.Join(os.WriteFile(filepath.Join(d, "adapters/fake/src/main.ts"), []byte("x"), 0o755),
				os.WriteFile(filepath.Join(d, "node/LICENSE"), []byte("x"), 0o644))
		}},
	}
	for _, c := range cases {
		dir := closureTree(t)
		if err := c.edit(dir); err != nil {
			t.Fatal(err)
		}
		if got := adapterDigest(t, dir); (got != base) != c.changes {
			t.Errorf("%s: digest %s, the copy's %s", c.name, got, base)
		}
	}
}

// What cannot be listed is refused: a FIFO, a missing closure path, a
// closure path under a symlinked parent, a name that would make the
// listing ambiguous, more entries than the bound, and a link whose target
// is absolute, unclean or lands outside the closure (#373 L1), in the root
// or out of it. @adc's two links land inside it and pass (closureTree).
func TestAdapterDigestRefuses(t *testing.T) {
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "bin/node"), []byte("node/bin/node\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := func(target, at string) func(d string) error {
		return func(d string) error {
			return errors.Join(os.RemoveAll(filepath.Join(d, at)), os.Symlink(target, filepath.Join(d, at)))
		}
	}
	refused := map[string]func(dir string) error{
		"a FIFO":           func(d string) error { return syscall.Mkfifo(filepath.Join(d, "node_modules/ajv/fifo"), 0o644) },
		"a missing gen/ts": func(d string) error { return os.RemoveAll(filepath.Join(d, "gen/ts")) },
		"a missing Node":   func(d string) error { return os.Remove(filepath.Join(d, "node/bin/node")) },
		"a symlinked node/ (in the root)": func(d string) error {
			return errors.Join(os.Rename(filepath.Join(d, "node"), filepath.Join(d, "real")), os.Symlink("real", filepath.Join(d, "node")))
		},
		"a symlinked node/ (out of it)":   link(outside, "node"),
		"gen/ts linked out of the root":   link(outside, "gen/ts"),
		"an absolute target in the root":  link(filepath.Join("/", "gen/ts"), "node_modules/@adc/contract-gen"),
		"a link to adapters/fake":         link("../adapters/fake", "node_modules/ajv"),
		"a link climbing out of the root": link("../../../../etc", "node_modules/@adc/contract"),
		"an unclean target":               link("../../gen/ts/../ts", "node_modules/@adc/contract-gen"),
		"gen/ts linked to adapters/fake":  link("../adapters/fake", "gen/ts"),
		"a space in a name":               func(d string) error { return os.WriteFile(filepath.Join(d, "gen/ts/a b"), nil, 0o644) },
		"a newline in a link":             func(d string) error { return os.Symlink("a\nf b", filepath.Join(d, "gen/ts/l")) },
		"too many entries": func(d string) error {
			many := filepath.Join(d, "node_modules/many")
			err := os.Mkdir(many, 0o755)
			for i := 0; err == nil && i < maxAdapterEntries; i++ {
				err = os.WriteFile(filepath.Join(many, strconv.Itoa(i)), nil, 0o644)
			}
			return err
		},
	}
	for name, edit := range refused {
		dir := closureTree(t)
		if err := edit(dir); err != nil {
			t.Fatal(err)
		}
		if d, err := AdapterDigest(dir); err == nil {
			t.Errorf("%s: digest %s", name, d)
		}
	}
	if _, err := AdapterDigest(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("an absent root was digested")
	}
	// A link that lands inside the closure, through a climb or not, is listed.
	dir := closureTree(t)
	err := errors.Join(os.Symlink("index.js", filepath.Join(dir, "node_modules/ajv/main.js")),
		os.Symlink("../../node/bin/node", filepath.Join(dir, "gen/ts/node")))
	if err != nil {
		t.Fatal(err)
	}
	adapterDigest(t, dir)
}

// CheckAdapter passes only the image's own platform's entry, equal to the
// installed release's digest; a refusal is ErrAdapter, an ErrManifest with
// its own text (#373 L3).
func TestCheckAdapter(t *testing.T) {
	want := "sha256:" + hex.EncodeToString(make([]byte, 32))
	other := "sha256:" + hex.EncodeToString(sha256.New().Sum(nil))
	img := func(adapter map[string]string) Image {
		return Image{OS: "linux", Arch: "arm64", Manifest: Manifest{Toolchain: "go", Adapter: adapter}}
	}
	if err := img(map[string]string{"linux/arm64": want, "linux/amd64": other}).CheckAdapter(want); err != nil {
		t.Fatalf("its own platform's digest: %v", err)
	}
	refused := map[string]struct {
		adapter map[string]string
		want    string
	}{
		"no adapter":                 {nil, want},
		"another digest":             {map[string]string{"linux/arm64": other, "linux/amd64": want}, want},
		"only another platform's":    {map[string]string{"linux/amd64": want}, want},
		"the arch alone":             {map[string]string{"arm64": want}, want},
		"empty on both sides":        {map[string]string{"linux/arm64": ""}, ""},
		"not a digest on both sides": {map[string]string{"linux/arm64": "x"}, "x"},
	}
	for name, c := range refused {
		err := img(c.adapter).CheckAdapter(c.want)
		if !errors.Is(err, ErrAdapter) || !errors.Is(err, ErrManifest) || err.Error() != "sandbox: the image's adapter is not the installed release's" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A label with and without the adapter parses as Resolve parses it, and
// CheckManifest (the gate's) passes both: images prepared before P22a-5d
// keep passing the gate. The reader is strict (#373 L2): a duplicate key
// at any level, a key in another case, another key, or anything but one
// object is refused.
func TestManifestAdapterLabel(t *testing.T) {
	base := t.TempDir()
	mod := []byte("module example.com/m\n")
	if err := os.WriteFile(filepath.Join(base, "go.mod"), mod, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(mod)
	lock := `{"toolchain":"go","lockfiles":{"go.mod":"sha256:` + hex.EncodeToString(sum[:]) + `"}`
	digest := "sha256:" + hex.EncodeToString(make([]byte, 32))
	other := "sha256:" + hex.EncodeToString(sha256.New().Sum(nil))
	both := `{"linux/amd64":"` + digest + `","linux/arm64":"` + digest + `"}`
	for label, adapter := range map[string]map[string]string{
		lock + "}":                        nil,
		lock + `,"adapter":` + both + "}": {"linux/amd64": digest, "linux/arm64": digest},
	} {
		img := Image{OS: "linux", Arch: "amd64"}
		m, err := ParseManifest([]byte(label))
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		img.Manifest = m
		if err := img.CheckManifest("go", base, []string{"go.mod"}); err != nil {
			t.Errorf("%s: CheckManifest: %v", label, err)
		}
		if len(img.Manifest.Adapter) != len(adapter) || adapter != nil && img.Manifest.Adapter["linux/amd64"] != digest {
			t.Errorf("%s: adapter %v", label, img.Manifest.Adapter)
		}
		if err := img.CheckAdapter(digest); (err == nil) != (adapter != nil) {
			t.Errorf("%s: CheckAdapter: %v", label, err)
		}
	}
	for _, label := range []string{
		lock + `,"adapter":{"linux/amd64":"` + other + `","linux/amd64":"` + digest + `"}}`,
		lock + `,"adapter":{"linux/amd64":"` + other + `"},"adapter":{"linux/arm64":"` + digest + `"}}`,
		lock + `,"adapter":{"linux/amd64":"` + other + `"},"Adapter":` + both + "}",
		lock + `,"ADAPTER":` + both + "}",
		lock + `,"Toolchain":"go"}`,
		lock + `,"extra":1}`,
		`{"toolchain":"go","toolchain":"go","lockfiles":{}}`,
		lock + `,"adapter":` + both + "}{}",
		`[` + lock + "}]",
		lock + `,"adapter":["linux/amd64"]}`,
	} {
		if m, err := ParseManifest([]byte(label)); !errors.Is(err, ErrManifest) {
			t.Errorf("%s: %+v, %v", label, m, err)
		}
	}
}
