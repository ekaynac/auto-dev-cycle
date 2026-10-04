// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// valid is a complete profile for a Go service on GitHub.
const valid = `schema: adc.project-profile/v1
id: prj_01m3250vkwffc979nzjw0g2sf9
name: Example service
repo:
  url: https://github.com/example/service.git
  base_ref: refs/heads/main
forge:
  kind: github
  capabilities: {change_requests: true, protected_branches: true, required_checks: true}
  settings: {}
source_tests: {dev_dependencies: true, vcs_metadata: false}
package:
  recipe:
    - [go, build, -o, dist/service, ./cmd/service]
  outputs: [dist/service]
runtime_verification:
  command: [dist/service, --self-check]
  facts_from: production
toolchain:
  images:
    go: golang@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  dependency_recipe:
    lockfiles: [go.sum]
    command: [go, mod, download]
    registries: [goproxy]
gate:
  command: [go, test, -json, ./...]
  result_adapter: go_test_json
  baseline: {kind: measured, relock_requires: approval}
  environments:
    - name: production
      facts:
        vcs_metadata: false
        network: loopback
        runtime: {language_version: "1.27", os: linux, arch: amd64}
      owner: ops
  ownership_rules: default
integration:
  plugin: docker-compose
  topology:
    files: [deploy/compose.yaml]
    adopted_revision: git:cccccccccccccccccccccccccccccccccccccccc
    digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  receipts_required: [smoke]
plugins: {deploy: docker-compose, records: json_ledger}
protected_paths: [docs/decisions/**, deploy/*.yaml, CANON.md]
protected_documents: [docs/decisions/*.md]
data_policy:
  floor: contracted
  routing: verified
  allowed_destinations: []
usage_profile_override: null
priority: 10
budget:
  rails: {per_task_usd: "5", per_day_usd: "50.00"}
retention:
  attempt_continuity_post_closure_ttl: P30D
  supervisor_notes_ttl: null
  blob_payload_ttl: P90D
vocabulary:
  task_id_pattern: '^SVC-[0-9]+$'
  branch_prefix: adc/
  language: en
`

func edit(old, new string) string {
	if !strings.Contains(valid, old) {
		panic("fixture edit anchor missing: " + old)
	}
	return strings.Replace(valid, old, new, 1)
}

func TestValidProfileLoads(t *testing.T) {
	p, err := Load([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if p.Repo.BaseRef != "refs/heads/main" || len(p.Gate.Environments) != 1 || p.Integration.Topology == nil {
		t.Fatalf("decoded %+v", p)
	}
	minimal := edit("  plugin: docker-compose\n  topology:\n    files: [deploy/compose.yaml]\n    adopted_revision: git:cccccccccccccccccccccccccccccccccccccccc\n    digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n",
		"  plugin: none\n  topology: null\n")
	minimal = strings.Replace(minimal, "  command: [dist/service, --self-check]\n  facts_from: production\n", "  command: null\n  facts_from: null\n", 1)
	if _, err := Load([]byte(minimal)); err != nil {
		t.Fatalf("no integration and no runtime verification: %v", err)
	}
}

func TestRefusals(t *testing.T) {
	cases := []struct{ name, doc, want string }{
		// The schema.
		{"unknown key", edit("priority: 10\n", "priority: 10\nnickname: x\n"), "nickname"},
		{"missing field", edit("priority: 10\n", ""), "priority"},
		{"wrong schema", edit("schema: adc.project-profile/v1", "schema: adc.project-profile/v2"), "schema"},
		{"unpinned image", edit("golang@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "golang:1.27"), "images"},
		{"empty gate command", edit("command: [go, test, -json, ./...]", "command: []"), "command"},
		{"empty executable", edit("command: [go, test, -json, ./...]", "command: ['', test]"), "names its executable"},
		{"NUL in an argument", edit("command: [go, test, -json, ./...]", `command: [go, "te\0st"]`), "NUL"},
		{"base ref with ..", edit("base_ref: refs/heads/main", "base_ref: refs/heads/a..b"), "'..'"},
		{"base ref ending .lock", edit("base_ref: refs/heads/main", "base_ref: refs/heads/main.lock"), ".lock"},
		{"base ref with a colon", edit("base_ref: refs/heads/main", "base_ref: 'refs/heads/a:b'"), "base_ref"},
		{"branch prefix with ..", edit("branch_prefix: adc/", "branch_prefix: adc../"), "branch_prefix"},
		{"branch prefix with a dot component", edit("branch_prefix: adc/", "branch_prefix: .adc/"), "branch_prefix"},
		{"custom tag on a mapping", edit("plugins: {deploy: docker-compose, records: json_ledger}", "plugins: !custom {deploy: docker-compose, records: json_ledger}"), "!custom"},
		{"str tag on a sequence", edit("outputs: [dist/service]", "outputs: !!str [dist/service]"), "!!str"},
		{"anchor on a key", edit("priority: 10\n", "&hidden priority: 10\n"), "anchors"},
		{"fractional priority", edit("priority: 10\n", "priority: 1000.0000000000000001\n"), "fractional"},
		{"tiny float", edit("priority: 10\n", "priority: 1e-1000\n"), "fractional"},
		{"hex integer", edit("priority: 10\n", "priority: 0x10\n"), "plain decimal"},
		{"negative zero", edit("priority: 10\n", "priority: -0\n"), "-0"},
		{"oversized", valid + "# " + strings.Repeat("x", MaxBytes) + "\n", "at most"},
		{"NUL in an output", edit("outputs: [dist/service]", `outputs: ["dist/ser\0vice"]`), "NUL"},
		{"NUL in a lockfile", edit("lockfiles: [go.sum]", `lockfiles: ["go\0.sum"]`), "NUL"},
		{"NUL in a topology file", edit("    files: [deploy/compose.yaml]\n", "    files: [\"deploy/comp\\0ose.yaml\"]\n"), "NUL"},
		{"branch prefix starting with -", edit("branch_prefix: adc/", "branch_prefix: -adc/"), "'-'"},
		{"base ref not a branch", edit("base_ref: refs/heads/main", "base_ref: main"), "base_ref"},
		{"topology without a plugin", edit("  plugin: docker-compose\n", "  plugin: none\n"), "topology"},
		{"plugin without a topology", edit("  topology:\n    files: [deploy/compose.yaml]\n    adopted_revision: git:cccccccccccccccccccccccccccccccccccccccc\n    digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n", "  topology: null\n"), "topology"},
		{"float amount", edit(`per_task_usd: "5"`, `per_task_usd: 5`), "per_task_usd"},
		{"three decimals", edit(`per_task_usd: "5"`, `per_task_usd: "5.001"`), "per_task_usd"},
		{"unquoted version", edit(`language_version: "1.27"`, `language_version: 1.27`), "1.27"},
		{"forge settings", edit("settings: {}", "settings: {token: x}"), "settings"},
		{"unknown registry", edit("registries: [goproxy]", "registries: [anywhere]"), "registries"},
		{"custom adapter name", edit("result_adapter: go_test_json", "result_adapter: 'custom:'"), "result_adapter"},
		// YAML.
		{"two documents", valid + "---\n" + valid, "exactly one YAML document"},
		{"duplicate key", edit("priority: 10\n", "priority: 10\npriority: 11\n"), "defined twice"},
		{"anchor and alias", edit("  outputs: [dist/service]\n", "  outputs: &o [dist/service]\n"), "anchors"},
		{"merge key", edit("plugins: {deploy: docker-compose, records: json_ledger}", "plugins: {<<: {deploy: none}, records: json_ledger}"), "merge key"},
		{"non-string key", edit("priority: 10\n", "priority: 10\n1: x\n"), "keys are strings"},
		{"binary tag", edit("language: en", "language: !!binary aGk="), "!!binary"},
		// Semantic checks.
		{"absolute output", edit("outputs: [dist/service]", "outputs: [/usr/bin/service]"), "relative"},
		{"escaping lockfile", edit("lockfiles: [go.sum]", "lockfiles: [../go.sum]"), "inside the repository"},
		{"unclean path", edit("outputs: [dist/service]", "outputs: [dist//service]"), "clean"},
		{"backslash path", edit("outputs: [dist/service]", `outputs: ['dist\service']`), "forward slashes"},
		{"bad glob", edit("CANON.md]", "'CANON[.md']"), "syntax error"},
		{"partial double star", edit("docs/decisions/**", "docs/decisions/a**"), "whole segment"},
		{"escaping glob", edit("CANON.md]", "../CANON.md]"), "inside the repository"},
		{"uncovered document", edit("protected_documents: [docs/decisions/*.md]", "protected_documents: [docs/notes/*.md]"), "not covered"},
		{"uncovered topology", edit("deploy/*.yaml", "deploy/*.yml"), "topology is protected"},
		{"spelling is not coverage", edit("protected_paths: [docs/decisions/**, deploy/*.yaml, CANON.md]", "protected_paths: ['docs/decisions/a?.md', deploy/*.yaml, CANON.md]"), "not covered"},
		{"bracket topology file", strings.Replace(edit("    files: [deploy/compose.yaml]\n", "    files: ['deploy/[ab].yaml']\n"), "deploy/*.yaml", "'deploy/[ab].yaml'", 1), "topology is protected"},
		{"NUL in a protected path", edit("CANON.md]", "\"CAN\\0ON.md\"]"), "NUL"},
		{"NUL in a protected document", edit("protected_documents: [docs/decisions/*.md]", "protected_documents: [\"docs/decisions/\\0.md\"]"), "NUL"},
		{"git with capabilities", edit("kind: github", "kind: git"), "plain git"},
		{"duplicate environment", edit("  ownership_rules: default\n", "    - name: production\n      facts: {vcs_metadata: false, network: none, runtime: {language_version: \"1\", os: linux, arch: amd64}}\n      owner: ops\n  ownership_rules: default\n"), "named twice"},
		{"unknown environment", edit("facts_from: production", "facts_from: staging"), "not a gate environment"},
		{"facts without command", edit("command: [dist/service, --self-check]", "command: null"), "exactly when"},
		{"task rail above day rail", edit(`per_task_usd: "5"`, `per_task_usd: "50.01"`), "exceeds"},
		{"bad task pattern", edit(`'^SVC-[0-9]+$'`, `'^SVC-[0-9+$'`), "task_id_pattern"},
		{"over-long task pattern", edit(`'^SVC-[0-9]+$'`, "'"+strings.Repeat("a", 300)+"'"), "at most"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load([]byte(c.doc))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

// Every semantic problem is reported, not only the first.
func TestCheckReportsEveryProblem(t *testing.T) {
	doc := edit("kind: github", "kind: git")
	doc = strings.Replace(doc, "facts_from: production", "facts_from: staging", 1)
	_, err := Load([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "plain git") || !strings.Contains(err.Error(), "not a gate environment") {
		t.Fatalf("got %v", err)
	}
}

func TestCents(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "5": 500, "5.5": 550, "50.01": 5001, "999999999.99": 99999999999} {
		if got, err := cents(in); err != nil || got != want {
			t.Errorf("cents(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}

// Astra #30: a JSON profile goes straight to strict decoding, so valid JSON
// (supplementary characters as surrogate-pair escapes) loads, and duplicate
// names are still refused.
func TestJSONProfiles(t *testing.T) {
	js, err := yamlToJSON([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.Replace(string(js), `"name":"Example service"`, `"name":"\ud83d\ude00 service"`, 1)
	if doc == string(js) {
		t.Fatal("fixture: name not found in the JSON form")
	}
	p, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("a JSON profile with a surrogate pair: %v", err)
	}
	if p.Name != "\U0001F600 service" {
		t.Fatalf("name %q", p.Name)
	}
	dup := strings.Replace(doc, `"priority":10`, `"priority":10,"priority":11`, 1)
	if _, err := Load([]byte(dup)); err == nil {
		t.Fatal("a JSON profile with a duplicate name loaded")
	}
	badUTF8 := strings.Replace(doc, `"language":"en"`, "\"language\":\"e\xffn\"", 1)
	if badUTF8 == doc {
		t.Fatal("fixture: language not found")
	}
	if _, err := Load([]byte(badUTF8)); err == nil {
		t.Fatal("a JSON profile with invalid UTF-8 loaded")
	}
	neg := strings.Replace(doc, `"priority":10`, `"priority":-0`, 1)
	if _, err := Load([]byte(neg)); err == nil {
		t.Fatal("a JSON profile with -0 loaded")
	}
}

// Accepted forms: an empty argument after the executable and unconventional
// but valid branch names.
func TestAcceptedForms(t *testing.T) {
	doc := edit("command: [go, test, -json, ./...]", "command: [git, -c, 'core.hooksPath=', config, x.y, '']")
	doc = strings.Replace(doc, "base_ref: refs/heads/main", "base_ref: refs/heads/release+hotfix", 1)
	doc = strings.Replace(doc, "branch_prefix: adc/", "branch_prefix: team/ADC/", 1)
	if _, err := Load([]byte(doc)); err != nil {
		t.Fatal(err)
	}
}

// Astra #33: a YAML flow mapping (not valid JSON) is read as YAML.
func TestFlowStyleYAML(t *testing.T) {
	js, err := yamlToJSON([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	flow := regexp.MustCompile(`"([a-z_]+)":`).ReplaceAllString(string(js), "$1: ")
	if json.Valid([]byte(flow)) {
		t.Fatal("fixture: the flow form is still JSON")
	}
	if _, err := Load([]byte(flow)); err != nil {
		t.Fatalf("a flow-style YAML profile: %v", err)
	}
}

// Astra #33 round 3: a short pattern with counted repetitions is checked by
// parsing alone; it is never compiled, so it cannot make validation allocate.
func TestTaskPatternIsNotCompiled(t *testing.T) {
	big := edit(`'^SVC-[0-9]+$'`, `'(aaaaaaaaaa){1000}'`)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, err := Load([]byte(big)); err != nil {
		t.Fatalf("a valid pattern was refused: %v", err)
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 32<<20 {
		t.Fatalf("checking the pattern allocated %d bytes", grew)
	}
	nested := edit(`'^SVC-[0-9]+$'`, `'((a{1000}){1000}){1000}'`)
	if _, err := Load([]byte(nested)); err == nil {
		t.Fatal("a pattern whose expansion is too large was accepted")
	}
}

func segs(t *testing.T, g string) []string {
	t.Helper()
	s, err := compileGlob(g)
	if err != nil {
		t.Fatalf("%s: %v", g, err)
	}
	return s
}

func TestMatchesPath(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"docs/**", "docs/a/b.md", true},
		{"docs/**", "docs", true},
		{"**/*.md", "a/b/c.md", true},
		{"**/*.md", "c.md", true},
		{"docs/*.md", "docs/a/b.md", false},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/c", false},
		// Astra #30: a literal path is matched, never compared by spelling:
		// [ab].yaml matches a.yaml and b.yaml, not a file named "[ab].yaml".
		{"deploy/[ab].yaml", "deploy/a.yaml", true},
		{"deploy/[ab].yaml", "deploy/[ab].yaml", false},
	}
	for _, c := range cases {
		if got := matchesPath(segs(t, c.glob), strings.Split(c.path, "/")); got != c.want {
			t.Errorf("matchesPath(%q, %q) = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}

func TestCovers(t *testing.T) {
	cases := []struct {
		glob, doc string
		want      bool
	}{
		{"docs/**", "docs/*.md", true},
		{"docs/**", "docs/**", true},
		{"**", "a/**/b/*.md", true},
		{"docs/*", "docs/*.md", true},
		{"docs/*.md", "docs/*.md", true},
		{"docs/*.md", "docs/a.md", true},
		{"docs/[ab].md", "docs/[ab].md", true},
		{"docs/*", "docs/**", false},
		{"*.yaml", "*.yml", false},
		// Astra #30: matching a pattern's spelling is not containment.
		{"docs/a?.md", "docs/a*.md", false},
		{"docs/[ab].md", "docs/[ac].md", false},
		{"docs/a*", "docs/a?", false},
	}
	for _, c := range cases {
		if got := covers(segs(t, c.glob), segs(t, c.doc)); got != c.want {
			t.Errorf("covers(%q, %q) = %v, want %v", c.glob, c.doc, got, c.want)
		}
	}
}

// Astra #30: separated ** segments against a long non-matching path stay
// fast (memoised), and over-long globs are refused before matching.
func TestGlobMatchingIsBounded(t *testing.T) {
	g := segs(t, strings.Repeat("**/a/", 15)+"z")
	path := strings.Split(strings.Repeat("a/", 31)+"a", "/")
	done := make(chan bool, 1)
	go func() { done <- matchesPath(g, path) || covers(g, path) }()
	select {
	case got := <-done:
		if got {
			t.Fatal("matched a path that does not end in z")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("glob matching is not bounded")
	}
	if _, err := compileGlob(strings.Repeat("a/", 40) + "a"); err == nil {
		t.Fatal("a glob with too many segments")
	}
	if _, err := compileGlob(strings.Repeat("a", 300)); err == nil {
		t.Fatal("an over-long glob")
	}
}

// Astra #33: the coverage checks of a whole profile are bounded together:
// many expensive patterns are refused quickly, never matched for seconds.
func TestCoverageWorkIsBounded(t *testing.T) {
	var prot, docs []string
	for i := 0; i < 300; i++ {
		prot = append(prot, fmt.Sprintf("'%sz%d'", strings.Repeat("**/a/", 12), i))
		docs = append(docs, fmt.Sprintf("'%sb%d'", strings.Repeat("a/", 30), i))
	}
	prot = append(prot, "'**'")
	doc := edit("protected_paths: [docs/decisions/**, deploy/*.yaml, CANON.md]", "protected_paths: ["+strings.Join(prot, ", ")+"]")
	doc = strings.Replace(doc, "protected_documents: [docs/decisions/*.md]", "protected_documents: ["+strings.Join(docs, ", ")+"]", 1)
	if len(doc) > MaxBytes {
		t.Fatalf("fixture is %d bytes, over the input bound", len(doc))
	}
	start := time.Now()
	_, err := Load([]byte(doc))
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("validation took %v", took)
	}
	if err == nil || !strings.Contains(err.Error(), "steps") {
		t.Fatalf("want the coverage budget refusal, got %v", err)
	}
}

// Astra #33 round 2: the budget counts the bytes each segment comparison
// costs, not only table cells: long character classes against long names
// are refused quickly, through both coverage routes.
func TestCoverageBudgetCountsBytes(t *testing.T) {
	var prot, names []string
	for i := 0; i < 120; i++ {
		prot = append(prot, fmt.Sprintf("'*[%s]z%03d'", strings.Repeat("a", 238), i))
		names = append(names, fmt.Sprintf("'%s%03d'", strings.Repeat("a", 250), i))
	}
	prot = append(prot, "'**'")
	base := edit("protected_paths: [docs/decisions/**, deploy/*.yaml, CANON.md]", "protected_paths: ["+strings.Join(prot, ", ")+"]")
	topology := strings.Replace(base, "    files: [deploy/compose.yaml]\n", "    files: ["+strings.Join(names, ", ")+"]\n", 1)
	topology = strings.Replace(topology, "protected_documents: [docs/decisions/*.md]", "protected_documents: []", 1)
	documents := strings.Replace(base, "protected_documents: [docs/decisions/*.md]", "protected_documents: ["+strings.Join(names, ", ")+"]", 1)
	for name, doc := range map[string]string{"topology files": topology, "protected documents": documents} {
		if len(doc) > MaxBytes {
			t.Fatalf("%s: fixture is %d bytes", name, len(doc))
		}
		start := time.Now()
		_, err := Load([]byte(doc))
		if took := time.Since(start); took > 500*time.Millisecond {
			t.Errorf("%s: validation took %v", name, took)
		}
		if err == nil || !strings.Contains(err.Error(), "steps") {
			t.Errorf("%s: want the coverage budget refusal, got %v", name, err)
		}
	}
}

// Astra #33 round 3: computing the budget is linear: thousands of globs in
// both protection arrays are refused (or checked) without a quadratic pass.
func TestCoverageBudgetIsLinear(t *testing.T) {
	doc := func(n int) string {
		var names []string
		for i := 0; i < n; i++ {
			names = append(names, fmt.Sprintf("p%d", i))
		}
		list := "[" + strings.Join(names, ", ") + "]"
		d := edit("protected_paths: [docs/decisions/**, deploy/*.yaml, CANON.md]", "protected_paths: "+list)
		d = strings.Replace(d, "protected_documents: [docs/decisions/*.md]", "protected_documents: "+list, 1)
		d = strings.Replace(d, "    files: [deploy/compose.yaml]\n", "    files: [p0]\n", 1)
		if len(d) > MaxBytes {
			t.Fatalf("fixture is %d bytes", len(d))
		}
		return d
	}
	// best is the fastest of three loads, so a busy runner's noise does not
	// decide the ratio.
	best := func(d string) (time.Duration, error) {
		var fastest time.Duration
		var err error
		for i := 0; i < 3; i++ {
			start := time.Now()
			_, err = Load([]byte(d))
			if took := time.Since(start); i == 0 || took < fastest {
				fastest = took
			}
		}
		return fastest, err
	}
	small, _ := best(doc(1000))
	large, err := best(doc(4000))
	// Linear work grows about 4x from 1000 to 4000 entries per array; a
	// pass over the pairs grows 16x. The ratio, not a wall-clock budget
	// (which a loaded runner misses), is the check; the absolute bound is
	// only a backstop.
	if ratio := float64(large) / float64(max(small, time.Microsecond)); ratio > 8 {
		t.Fatalf("validation grew %.1fx from 1000 to 4000 entries (%v, %v)", ratio, small, large)
	}
	if large > 2*time.Second {
		t.Fatalf("validation took %v", large)
	}
	if err == nil || !strings.Contains(err.Error(), "steps") {
		t.Fatalf("want the coverage budget refusal, got %v", err)
	}
	if w := coverageWork(maxCoverageWork, [][]string{{strings.Repeat("a", 200)}}, [][]string{{strings.Repeat("b", 200)}}); w != 202*202 { // weight = 1 + (200 bytes + 1)
		t.Fatalf("coverageWork = %d", w)
	}
	huge := make([][]string, 3000)
	for i := range huge {
		huge[i] = []string{strings.Repeat("a", 250)}
	}
	if w := coverageWork(maxCoverageWork, huge, huge); w != maxCoverageWork+1 {
		t.Fatalf("coverageWork does not saturate: %d", w)
	}
}
