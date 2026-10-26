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

// The walk (#226 entry 206; Astra #443 r2 High 1): a link's target is
// searched and reported by the link's own path; OWN is exempt by its own
// path only, never through a link or a linked directory; a link chain is
// followed; a cycle, a dangling link and an unreadable file end the search
// incomplete; a newline in a name is a name; every form is found alone.
func TestSearchWalk(t *testing.T) {
	const secret = "sk-filter-PROBE-0123456789abcdef"
	forms, err := SecretForms(secret)
	if err != nil {
		t.Fatal(err)
	}
	run := func(root string, own ...string) ([]string, error) {
		return Search{Forms: forms, Own: own}.Run([]string{root})
	}
	for name, c := range map[string]struct {
		build func(root string)
		own   string
		want  []string
	}{
		"clean":         {func(r string) { write(t, r+"/a/b", "nothing\n", 0o600) }, "", nil},
		"OWN alone":     {func(r string) { write(t, r+"/home/token", secret, 0o600) }, "home/token", nil},
		"a link to OWN": {func(r string) { write(t, r+"/home/token", secret, 0o600); link(t, "../home/token", r+"/out/copy") }, "home/token", []string{"out/copy"}},
		"OWN through a linked directory": {func(r string) {
			write(t, r+"/home/token", secret, 0o600)
			link(t, "../home", r+"/out/alias")
		}, "home/token", []string{"out/alias/token"}},
		"a link chain": {func(r string) {
			write(t, r+"/x/leak", secret, 0o600)
			link(t, "../x/leak", r+"/y/one")
			link(t, "one", r+"/y/two")
		}, "", []string{"x/leak", "y/one", "y/two"}},
		"a newline in a name": {func(r string) {
			write(t, r+"/home/token", secret, 0o600)
			write(t, r+"/home/token\n", secret, 0o600)
		}, "home/token", []string{"home/token\n"}},
		"its hex alone": {func(r string) { write(t, r+"/hex", "b24f9f8a5b5eafe3d138113ed35db7e39ebbf3d69b71d79f", 0o600) }, "", []string{"hex"}},
		"a form across two reads": {func(r string) {
			write(t, r+"/big", strings.Repeat("x", 1<<20-5)+secret, 0o600)
		}, "", []string{"big"}},
	} {
		root := t.TempDir()
		c.build(root)
		var own []string
		if c.own != "" {
			own = []string{filepath.Join(root, c.own)}
		}
		got, err := run(root, own...)
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
		if got, err := run(root); err != nil || len(got) != 1 {
			t.Errorf("the form %q alone: %v %q", f, err, got)
		}
	}
	// Never a skip: a cycle, a dangling link, an unreadable file or directory.
	for name, build := range map[string]func(root string){
		"a cycle":            func(r string) { link(t, "..", r+"/d/up") },
		"a dangling link":    func(r string) { link(t, "nowhere", r+"/d/gone") },
		"a link to itself":   func(r string) { link(t, "self", r+"/self") },
		"an unreadable file": func(r string) { write(t, r+"/"+secret, "x", 0o000) },
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
		if got, err := run(root); !errors.Is(err, ErrIncomplete) || got != nil {
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
