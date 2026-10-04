// SPDX-License-Identifier: Apache-2.0

package profile

import (
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
		{"spelling is not coverage", edit("protected_paths: [docs/decisions/**, deploy/*.yaml, CANON.md]", "protected_paths: ['docs/decisions/a?.md', deploy/*.yaml, CANON.md]"), "not covered"},
		{"bracket topology file", strings.Replace(edit("    files: [deploy/compose.yaml]\n", "    files: ['deploy/[ab].yaml']\n"), "deploy/*.yaml", "'deploy/[ab].yaml'", 1), "topology is protected"},
		{"custom tag on a mapping", edit("plugins: {deploy: docker-compose, records: json_ledger}", "plugins: !custom {deploy: docker-compose, records: json_ledger}"), "!custom"},
		{"str tag on a sequence", edit("outputs: [dist/service]", "outputs: !!str [dist/service]"), "!!str"},
		{"anchor on a key", edit("priority: 10\n", "&hidden priority: 10\n"), "anchors"},
		{"fractional priority", edit("priority: 10\n", "priority: 1000.0000000000000001\n"), "fractional"},
		{"tiny float", edit("priority: 10\n", "priority: 1e-1000\n"), "fractional"},
		{"hex integer", edit("priority: 10\n", "priority: 0x10\n"), "plain decimal"},
		{"negative zero", edit("priority: 10\n", "priority: -0\n"), "-0"},
		{"oversized", valid + "# " + strings.Repeat("x", MaxBytes) + "\n", "at most"},
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
		{"git with capabilities", edit("kind: github", "kind: git"), "plain git"},
		{"duplicate environment", edit("  ownership_rules: default\n", "    - name: production\n      facts: {vcs_metadata: false, network: none, runtime: {language_version: \"1\", os: linux, arch: amd64}}\n      owner: ops\n  ownership_rules: default\n"), "named twice"},
		{"unknown environment", edit("facts_from: production", "facts_from: staging"), "not a gate environment"},
		{"facts without command", edit("command: [dist/service, --self-check]", "command: null"), "exactly when"},
		{"task rail above day rail", edit(`per_task_usd: "5"`, `per_task_usd: "50.01"`), "exceeds"},
		{"bad task pattern", edit(`'^SVC-[0-9]+$'`, `'^SVC-[0-9+$'`), "task_id_pattern"},
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
