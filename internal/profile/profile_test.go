// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"encoding/json"
	"regexp"
	"runtime"
	"strings"
	"testing"
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
