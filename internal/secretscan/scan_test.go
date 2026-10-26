// SPDX-License-Identifier: Apache-2.0

package secretscan

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The forms are the shell's (secrets.sh's forms, recorded before it was
// removed for this port; #226 entry 206), as a set.
func TestSecretFormsAreTheShells(t *testing.T) {
	for secret, want := range map[string][]string{
		"sk-filter-PROBE-0123456789abcdef": {"sk-filter-PROBE-0123456789abcdef", "sk+filter+PROBE+0123456789abcdef", "sk-filter-PROBE-",
			"0123456789abcdef", "c2stZmlsdGVyLVBST0JFLTAxMjM0NTY3ODlhYmNkZWY=", "b24f9f8a5b5eafe3d138113ed35db7e39ebbf3d69b71d79f"},
		"tok_abc123.Zm9vYmFyYmF6cXV4LXF1dXgtMTIzNDU2": {"tok_abc123.Zm9vYmFyYmF6cXV4LXF1dXgtMTIzNDU2", "Zm9vYmFyYmF6cXV4LXF1dXgtMTIzNDU2",
			"Zm9vYmFyYmF6cXV4", "LXF1dXgtMTIzNDU2", "dG9rX2FiYzEyMy5abTl2WW1GeVltRjZjWFY0TFhGMWRYZ3RNVEl6TkRVMg==",
			"666f6f62617262617a7175782d717575782d313233343536"},
		// No base64 by its text: no hex form.
		"p@ss word! 0123456789abcdef": {"p@ss word! 0123456789abcdef", "p@ss word! 01234", "0123456789abcdef", "cEBzcyB3b3JkISAwMTIzNDU2Nzg5YWJjZGVm"},
	} {
		got, err := SecretForms(secret)
		slices.Sort(got)
		slices.Sort(want)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%q: %v %q, want %q", secret, err, got, want)
		}
	}
	if _, err := SecretForms("short-secret"); !errors.Is(err, ErrShort) {
		t.Errorf("a secret shorter than 16: %v", err)
	}
}

// cycle-expect.mjs's canary forms, ported: JSON (JavaScript's) and Go's
// escaping, once and twice, and their compositions, of the line and its
// trimmed core.
func TestCanaryForms(t *testing.T) {
	const canary = "\tconst q7vCanary = \"ADC-<q7v>-0123456789abcdef\""
	got, err := CanaryForms(canary)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		canary,
		strings.TrimSpace(canary),
		`\tconst q7vCanary = \"ADC-<q7v>-0123456789abcdef\"`,
		`\\tconst q7vCanary = \\\"ADC-<q7v>-0123456789abcdef\\\"`,
		`\tconst q7vCanary = \"ADC-\u003cq7v\u003e-0123456789abcdef\"`,
		`\\tconst q7vCanary = \\\"ADC-\u003cq7v\u003e-0123456789abcdef\\\"`,
		`const q7vCanary = \"ADC-\u003cq7v\u003e-0123456789abcdef\"`,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("no form %q in %q", want, got)
		}
	}
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func link(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// The walk (#226 entry 206; the lead's ruling on Astra #443 r2 High 1): no
// link is followed and only regular files are read, each at its real path.
// OWN is exempt at its own path only; a link that resolves to OWN, a chain
// ending there, or a link to a directory holding it, is a finding by the
// link's own path. In the run's own directory (LinksInside) a link out of
// the roots ends the search incomplete; under a sandbox root a dangling or
// absolute system link is only recorded. An unreadable file or directory
// ends the search; a newline in a name is a name; every form is found
// alone.
func TestSearchWalk(t *testing.T) {
	const secret = "sk-filter-PROBE-0123456789abcdef"
	forms, err := SecretForms(secret)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		build  func(root string)
		own    string
		inside bool
		want   []string
	}{
		"clean":         {func(r string) { write(t, r+"/a/b", "nothing\n", 0o600) }, "", true, nil},
		"OWN alone":     {func(r string) { write(t, r+"/home/token", secret, 0o600) }, "home/token", true, nil},
		"a link to OWN": {func(r string) { write(t, r+"/home/token", secret, 0o600); link(t, "../home/token", r+"/out/copy") }, "home/token", true, []string{"out/copy"}},
		"a chain to OWN": {func(r string) {
			write(t, r+"/home/token", secret, 0o600)
			link(t, "../home/token", r+"/y/one")
			link(t, "one", r+"/y/two")
		}, "home/token", true, []string{"y/one", "y/two"}},
		"a link to OWN's directory": {func(r string) {
			write(t, r+"/home/token", secret, 0o600)
			link(t, "../home", r+"/out/alias")
		}, "home/token", true, []string{"out/alias"}},
		"a link to a leak, read where it is": {func(r string) {
			write(t, r+"/x/leak", secret, 0o600)
			link(t, "../x/leak", r+"/y/one")
		}, "", true, []string{"x/leak"}},
		"a link to OWN under a sandbox root":   {func(r string) { write(t, r+"/home/token", secret, 0o600); link(t, "../home/token", r+"/out/copy") }, "home/token", false, []string{"out/copy"}},
		"a cycle, never followed":              {func(r string) { link(t, "..", r+"/d/up") }, "", true, nil},
		"a link to itself":                     {func(r string) { link(t, "self", r+"/self") }, "", true, nil},
		"a dangling link inside":               {func(r string) { link(t, "nowhere", r+"/d/gone") }, "", true, nil},
		"a dangling link under a sandbox root": {func(r string) { link(t, "/nonexistent/adc-scan-probe", r+"/layer/etc/gone") }, "", false, nil},
		"an absolute system link under a sandbox root": {func(r string) {
			link(t, "/usr/bin", r+"/layer/bin")
			link(t, "/etc/hostname", r+"/layer/etc/hostname")
		}, "", false, nil},
		"a newline in a name": {func(r string) {
			write(t, r+"/home/token", secret, 0o600)
			write(t, r+"/home/token\n", secret, 0o600)
		}, "home/token", true, []string{"home/token\n"}},
		"its hex alone": {func(r string) { write(t, r+"/hex", "b24f9f8a5b5eafe3d138113ed35db7e39ebbf3d69b71d79f", 0o600) }, "", true, []string{"hex"}},
		// The first read takes 1 MiB and the longest form less one byte: the secret straddles its end.
		"a form across two reads": {func(r string) {
			longest := 0
			for _, f := range forms {
				longest = max(longest, len(f))
			}
			write(t, r+"/big", strings.Repeat("x", 1<<20+longest-1-5)+secret+strings.Repeat("y", 100), 0o600)
		}, "", true, []string{"big"}},
	} {
		root := t.TempDir()
		c.build(root)
		var own []string
		if c.own != "" {
			own = []string{filepath.Join(root, c.own)}
		}
		got, err := Search{Forms: forms, Own: own, LinksInside: c.inside}.Run([]string{root})
		var want []string
		for _, w := range c.want {
			want = append(want, filepath.Join(root, w))
		}
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: %v %q, want %q", name, err, got, want)
		}
	}
	// Each form alone is found.
	for _, f := range forms {
		root := t.TempDir()
		write(t, root+"/f", "x "+f+" y", 0o600)
		if got, err := (Search{Forms: forms}).Run([]string{root}); err != nil || len(got) != 1 {
			t.Errorf("the form %q alone: %v %q", f, err, got)
		}
	}
	// Never a skip: an unreadable file or directory; in the run's own directory, a link out of it.
	for name, build := range map[string]func(root string){
		"a link out of the roots": func(r string) { link(t, "/etc/hostname", r+"/out/host") },
		"a dangling link out":     func(r string) { link(t, "/nonexistent/adc-scan-probe", r+"/out/gone") },
		"an unreadable file":      func(r string) { write(t, r+"/"+secret, "x", 0o000) },
		"an unreadable directory": func(r string) {
			if err := os.MkdirAll(r+"/locked", 0o000); err != nil {
				t.Fatal(err)
			}
		},
	} {
		if os.Getuid() == 0 && strings.HasPrefix(name, "an unreadable") {
			continue
		}
		root := t.TempDir()
		build(root)
		if got, err := (Search{Forms: forms, LinksInside: true}).Run([]string{root}); !errors.Is(err, ErrIncomplete) || got != nil {
			t.Errorf("%s: %v %q", name, err, got)
		}
		_ = os.Chmod(filepath.Join(root, "locked"), 0o700)
	}
}

// A name search (canary_path): an entry of the name below a root, of any
// kind (a dangling link too: it follows no link), at most Depth below it.
func TestSearchNames(t *testing.T) {
	root := t.TempDir()
	write(t, root+"/a/b/canary-x", "", 0o600)
	link(t, "nowhere", root+"/c/canary-x")
	if err := os.MkdirAll(root+"/canary-x", 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := Search{Names: []string{"canary-x"}}.Run([]string{root})
	want := []string{root + "/a/b/canary-x", root + "/c/canary-x", root + "/canary-x"}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("under: %v %q", err, got)
	}
	got, err = Search{Names: []string{"canary-x"}, Depth: 1}.Run([]string{root})
	if err != nil || !slices.Equal(got, []string{root + "/canary-x"}) {
		t.Errorf("at depth 1: %v %q", err, got)
	}
	if got, err := (Search{Names: []string{"canary-x"}}).Run([]string{root + "/canary-x"}); err != nil || got != nil {
		t.Errorf("the root itself is no finding: %v %q", err, got)
	}
}

// A canary search decodes JSON: in a request's body, a tool call's
// argument string, four strings deep, Go-escaped.
func TestSearchDecodesTheCanary(t *testing.T) {
	const canary = "\tconst q7vCanary = \"ADC-<q7v>-0123456789abcdef\""
	forms, err := CanaryForms(canary)
	if err != nil {
		t.Fatal(err)
	}
	str := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	deep := canary
	for range 4 {
		deep = str(map[string]any{"content": deep})
	}
	for name, body := range map[string]string{
		"a body":            str(map[string]any{"messages": []any{map[string]any{"content": "x\n" + canary}}}),
		"an argument":       str(map[string]any{"arguments": str(map[string]any{"content": canary})}),
		"four strings deep": deep,
		"a log line":        "adcd: " + str(canary),
	} {
		root := t.TempDir()
		write(t, root+"/f", body, 0o600)
		if got, err := (Search{Forms: forms, Decode: true}).Run([]string{root}); err != nil || len(got) != 1 {
			t.Errorf("%s: %v %q", name, err, got)
		}
	}
	root := t.TempDir()
	write(t, root+"/f", str(map[string]any{"content": "nothing"}), 0o600)
	if got, err := (Search{Forms: forms, Decode: true}).Run([]string{root}); err != nil || got != nil {
		t.Errorf("clean: %v %q", err, got)
	}
}
