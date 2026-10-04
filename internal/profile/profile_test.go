// SPDX-License-Identifier: Apache-2.0

package profile

import (
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
		{"empty argument", edit("command: [go, test, -json, ./...]", "command: [go, '']"), "command"},
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

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"docs/**", "docs/a/b.md", true},
		{"docs/**", "docs", true},
		{"**/*.md", "a/b/c.md", true},
		{"**/*.md", "c.md", true},
		{"docs/*.md", "docs/a/b.md", false},
		{"docs/*.md", "docs/*.md", true},
		{"docs/**", "docs/*.md", true},
		{"docs/*", "docs/**", false},
		{"*.yaml", "*.yml", false},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/c", false},
		// A bracket pattern does not match its own spelling; it covers itself
		// by identity.
		{"docs/[ab].md", "docs/[ab].md", true},
		{"docs/[ab].md", "docs/[ac].md", false},
	}
	for _, c := range cases {
		segs, err := compileGlob(c.glob)
		if err != nil {
			t.Fatalf("%s: %v", c.glob, err)
		}
		if got := globMatch(segs, strings.Split(c.path, "/")); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}

func TestCents(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "5": 500, "5.5": 550, "50.01": 5001, "999999999.99": 99999999999} {
		if got, err := cents(in); err != nil || got != want {
			t.Errorf("cents(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}
