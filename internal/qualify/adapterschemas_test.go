// SPDX-License-Identifier: Apache-2.0

package qualify

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// #226 entry 191: the adapter's two digests cover the adapter wire
// contract's own validators (gen/ts/adapter-validators.cjs: schemas/adapter/*
// and what they reference), never every schema's validators.cjs. gen/ts is
// regenerated (adapters/contract/scripts/gen.mjs) from a copy of schemas/:
//   - qualify.AdapterDigest, over the repository's adapter paths with that
//     gen/ts;
//   - sandbox's digest of gen/ts as a release ships it (dist-adapters.sh's
//     committed gen/ts files, the only part of sandbox.AdapterClosure a
//     schema reaches).
//
// A change to another schema (api/task.json's description) moves neither,
// nor does one to a shared definition no adapter schema references
// (common/defs.json's GrantID); a change to an adapter schema
// (adapter/outcome.json), to a shared definition one references
// (AttemptID's pattern, inlined) or to the reason list (vocab/reasons.yaml,
// through formats.cjs) moves both (Opus #428 L2). The release ships no
// validators.cjs.
func TestAdapterDigestsIgnoreOtherSchemas(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH: the adapter tests run under make test (the test sandbox provides /opt/node/bin/node)")
	}
	raw, err := os.ReadFile(filepath.Join(root, "scripts", "dist-adapters.sh"))
	list := string(raw)
	i := strings.Index(list, "committed=(")
	if err != nil || i < 0 || !strings.Contains(list[i:], ")") {
		t.Fatalf("dist-adapters.sh has no committed list: %v", err)
	}
	var shipped []string
	for _, f := range strings.Fields(list[i+len("committed=(") : i+strings.Index(list[i:], ")")]) {
		if strings.HasPrefix(f, "gen/ts/") {
			shipped = append(shipped, f)
		}
	}
	if slices.Contains(shipped, "gen/ts/validators.cjs") || !slices.Contains(shipped, "gen/ts/adapter-validators.cjs") {
		t.Fatalf("the release ships %v of gen/ts", shipped)
	}
	// edit replaces old with new, once, in the schemas copy's file f.
	edit := func(f, old, new string) func(string) {
		return func(schemas string) {
			p := filepath.Join(schemas, f)
			b, err := os.ReadFile(p)
			if err == nil && strings.Count(string(b), old) != 1 {
				err = errors.New(f + ": the edit's anchor is not there once")
			}
			if err == nil {
				err = os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	described := func(f string) func(string) {
		return func(schemas string) {
			p := filepath.Join(schemas, f)
			var doc map[string]any
			b, err := os.ReadFile(p)
			if err == nil {
				err = json.Unmarshal(b, &doc)
			}
			if err == nil {
				doc["description"] = doc["description"].(string) + " Changed."
				b, err = json.Marshal(doc)
			}
			if err == nil {
				err = os.WriteFile(p, b, 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	digests := func(change func(string)) (string, string) {
		t.Helper()
		schemas, lib, release := t.TempDir(), t.TempDir(), t.TempDir()
		if err := copyTree(filepath.Join(root, "schemas"), schemas); err != nil {
			t.Fatal(err)
		}
		if change != nil {
			change(schemas)
		}
		for _, p := range adapterPaths {
			if strings.HasPrefix(p, "gen/ts/") {
				continue
			}
			src, dst := filepath.Join(root, p), filepath.Join(lib, p)
			fi, err := os.Stat(src)
			if err == nil && fi.IsDir() {
				err = copyTree(src, dst)
			} else if err == nil {
				var b []byte
				if b, err = os.ReadFile(src); err == nil {
					err = errors.Join(os.MkdirAll(filepath.Dir(dst), 0o755), os.WriteFile(dst, b, 0o644))
				}
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		gen := exec.Command(node, filepath.Join(root, "adapters/contract/scripts/gen.mjs"), filepath.Join(lib, "gen/ts"), schemas)
		if out, err := gen.CombinedOutput(); err != nil {
			t.Fatalf("gen.mjs: %v: %s", err, out)
		}
		q, err := AdapterDigest(lib)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range shipped {
			b, err := os.ReadFile(filepath.Join(lib, f))
			if err == nil {
				err = errors.Join(os.MkdirAll(filepath.Dir(filepath.Join(release, f)), 0o755), os.WriteFile(filepath.Join(release, f), b, 0o644))
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		s, err := sandbox.ClosureDigest(release, []string{"gen/ts"})
		if err != nil {
			t.Fatal(err)
		}
		return q, s
	}
	q, s := digests(nil)
	if real, err := AdapterDigest(root); err != nil || real != q {
		t.Fatalf("the regenerated adapter's digest %s, the repository's %s (%v): make gen", q, real, err)
	}
	for name, c := range map[string]struct {
		change func(string)
		moves  bool
	}{
		"api/task.json's description":          {described("api/task.json"), false},
		"an unreferenced definition (GrantID)": {edit("common/defs.json", `"pattern": "^grt_"`, `"pattern": "^grt_x"`), false},
		"adapter/outcome.json's description":   {described("adapter/outcome.json"), true},
		"a referenced definition (AttemptID)":  {edit("common/defs.json", `"pattern": "^att_"`, `"pattern": "^att_x"`), true},
		"a new reason (vocab/reasons.yaml)":    {edit("vocab/reasons.yaml", "reasons:\n", "reasons:\n  - {code: probe_reason, outcomes: [failed], domain: cycle, introduced: 0.1.0, deprecated: null, source: \"02\", description: \"A probe.\"}\n"), true},
	} {
		q2, s2 := digests(c.change)
		if (q2 != q) != c.moves || (s2 != s) != c.moves {
			t.Errorf("%s: qualify moved %v, sandbox moved %v; want both %v", name, q2 != q, s2 != s, c.moves)
		}
	}
}
