// SPDX-License-Identifier: Apache-2.0

package schemas_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

// profile returns a valid project profile (07).
func profile() map[string]any {
	argv := func(a ...any) []any { return a }
	return map[string]any{
		"schema": "adc.project-profile/v1",
		"id":     prjID,
		"name":   "Example service",
		"repo":   map[string]any{"url": "https://github.com/example/service.git", "base_ref": "refs/heads/main"},
		"forge": map[string]any{"kind": "github", "settings": map[string]any{},
			"capabilities": map[string]any{"change_requests": true, "protected_branches": true, "required_checks": true}},
		"source_tests":         map[string]any{"dev_dependencies": true, "vcs_metadata": false},
		"package":              map[string]any{"recipe": []any{argv("go", "build", "./...")}, "outputs": []any{"dist/service"}},
		"runtime_verification": map[string]any{"command": argv("dist/service", "--self-check"), "facts_from": "production"},
		"toolchain": map[string]any{
			"images":            map[string]any{"go": "golang@" + dgA},
			"dependency_recipe": map[string]any{"lockfiles": []any{"go.sum"}, "command": argv("go", "mod", "download"), "registries": []any{"goproxy"}},
		},
		"gate": map[string]any{
			"command": argv("go", "test", "./..."), "result_adapter": "go_test_json",
			"baseline": map[string]any{"kind": "measured", "relock_requires": "approval"},
			"environments": []any{map[string]any{"name": "production", "owner": "ops",
				"facts": map[string]any{"vcs_metadata": false, "network": "loopback",
					"runtime": map[string]any{"language_version": "1.27", "os": "linux", "arch": "amd64"}}}},
			"ownership_rules": "default",
		},
		"integration": map[string]any{"plugin": "docker-compose", "receipts_required": []any{"smoke"},
			"topology": map[string]any{"files": []any{"deploy/compose.yaml"}, "adopted_revision": git, "digest": dgB}},
		"plugins":                map[string]any{"deploy": "docker-compose", "records": "json_ledger"},
		"protected_paths":        []any{"docs/decisions/**", "deploy/*.yaml"},
		"protected_documents":    []any{"docs/decisions/*.md"},
		"data_policy":            map[string]any{"floor": "contracted", "routing": "verified", "allowed_destinations": []any{}},
		"usage_profile_override": nil,
		"priority":               10,
		"budget":                 map[string]any{"rails": map[string]any{"per_task_usd": "5", "per_day_usd": "50.00"}},
		"retention":              map[string]any{"attempt_continuity_post_closure_ttl": "P30D", "supervisor_notes_ttl": nil, "blob_payload_ttl": "P90D"},
		"vocabulary":             map[string]any{"task_id_pattern": "^SVC-[0-9]+$", "branch_prefix": "adc/", "language": "en"},
	}
}

func TestProjectProfileSchema(t *testing.T) {
	ok := func(p map[string]any) error {
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		_, err = contract.Decode[types.ProjectProfile]("config/project-profile", b)
		return err
	}
	if err := ok(profile()); err != nil {
		t.Fatalf("valid profile: %v", err)
	}
	none := profile()
	none["integration"] = map[string]any{"plugin": "none", "topology": nil, "receipts_required": []any{}}
	none["runtime_verification"] = map[string]any{"command": nil, "facts_from": nil}
	if err := ok(none); err != nil {
		t.Fatalf("no integration, no runtime verification: %v", err)
	}

	cases := map[string]func(p map[string]any){
		"unknown key":   func(p map[string]any) { p["nickname"] = "x" },
		"missing field": func(p map[string]any) { delete(p, "retention") },
		"absent null":   func(p map[string]any) { delete(p, "usage_profile_override") },
		"other version": func(p map[string]any) { p["schema"] = "adc.project-profile/v2" },
		"unpinned image": func(p map[string]any) {
			p["toolchain"].(map[string]any)["images"] = map[string]any{"go": "golang:1.27"}
		},
		"empty gate command": func(p map[string]any) {
			p["gate"].(map[string]any)["command"] = []any{}
		},
		"no environment": func(p map[string]any) { p["gate"].(map[string]any)["environments"] = []any{} },
		"base ref not a branch": func(p map[string]any) {
			p["repo"].(map[string]any)["base_ref"] = "main"
		},
		"base ref with a space": func(p map[string]any) {
			p["repo"].(map[string]any)["base_ref"] = "refs/heads/my branch"
		},
		"no image": func(p map[string]any) { p["toolchain"].(map[string]any)["images"] = map[string]any{} },
		"nested unknown key": func(p map[string]any) {
			p["gate"].(map[string]any)["baseline"].(map[string]any)["window"] = 3
		},
		"unknown key in an environment": func(p map[string]any) {
			p["gate"].(map[string]any)["environments"].([]any)[0].(map[string]any)["region"] = "eu"
		},
		"malformed topology revision": func(p map[string]any) {
			p["integration"].(map[string]any)["topology"].(map[string]any)["adopted_revision"] = "HEAD"
		},
		"malformed topology digest": func(p map[string]any) {
			p["integration"].(map[string]any)["topology"].(map[string]any)["digest"] = "md5:00"
		},
		"empty topology files": func(p map[string]any) {
			p["integration"].(map[string]any)["topology"].(map[string]any)["files"] = []any{}
		},
		"data floor": func(p map[string]any) { p["data_policy"].(map[string]any)["floor"] = "anyone" },
		"routing":    func(p map[string]any) { p["data_policy"].(map[string]any)["routing"] = "trusted" },
		"destination": func(p map[string]any) {
			p["data_policy"].(map[string]any)["allowed_destinations"] = []any{"Not A Slug"}
		},
		"usage profile": func(p map[string]any) { p["usage_profile_override"] = "everything" },
		"network": func(p map[string]any) {
			p["gate"].(map[string]any)["environments"].([]any)[0].(map[string]any)["facts"].(map[string]any)["network"] = "internet"
		},
		"absent nullable ttl": func(p map[string]any) { delete(p["retention"].(map[string]any), "blob_payload_ttl") },
		"absent nullable task pattern": func(p map[string]any) {
			delete(p["vocabulary"].(map[string]any), "task_id_pattern")
		},
		"forge settings": func(p map[string]any) {
			p["forge"].(map[string]any)["settings"] = map[string]any{"token": "x"}
		},
		"topology without a plugin": func(p map[string]any) {
			p["integration"].(map[string]any)["plugin"] = "none"
		},
		"plugin without a topology": func(p map[string]any) {
			p["integration"].(map[string]any)["topology"] = nil
		},
		"float amount": func(p map[string]any) { p["budget"].(map[string]any)["rails"].(map[string]any)["per_task_usd"] = 5 },
		"three decimals": func(p map[string]any) {
			p["budget"].(map[string]any)["rails"].(map[string]any)["per_task_usd"] = "5.001"
		},
		"leading zero":   func(p map[string]any) { p["budget"].(map[string]any)["rails"].(map[string]any)["per_task_usd"] = "05" },
		"priority bound": func(p map[string]any) { p["priority"] = 1001 },
		"unknown registry": func(p map[string]any) {
			p["toolchain"].(map[string]any)["dependency_recipe"].(map[string]any)["registries"] = []any{"anywhere"}
		},
		"custom adapter without a name": func(p map[string]any) {
			p["gate"].(map[string]any)["result_adapter"] = "custom:"
		},
		"branch prefix without a slash": func(p map[string]any) {
			p["vocabulary"].(map[string]any)["branch_prefix"] = "adc"
		},
		"empty command": func(p map[string]any) {
			p["toolchain"].(map[string]any)["dependency_recipe"].(map[string]any)["command"] = []any{}
		},
		"zero-length ttl": func(p map[string]any) {
			p["retention"].(map[string]any)["attempt_continuity_post_closure_ttl"] = "P"
		},
	}
	for name, mutate := range cases {
		p := profile()
		mutate(p)
		if err := ok(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Accepted forms: a custom adapter with a name, an empty argument after
	// the executable, unconventional branch names, every nullable as null.
	p := profile()
	p["gate"].(map[string]any)["result_adapter"] = "custom:pytest-json"
	p["gate"].(map[string]any)["command"] = []any{"git", "-c", "core.hooksPath=", "config", "x.y", ""}
	p["repo"].(map[string]any)["base_ref"] = "refs/heads/release+hotfix"
	p["vocabulary"].(map[string]any)["branch_prefix"] = "team/ADC/"
	p["usage_profile_override"] = "on-prem"
	if err := ok(p); err != nil {
		t.Fatalf("accepted forms: %v", err)
	}
	nulls := profile()
	nulls["retention"] = map[string]any{"attempt_continuity_post_closure_ttl": "P30D", "supervisor_notes_ttl": nil, "blob_payload_ttl": nil}
	nulls["vocabulary"].(map[string]any)["task_id_pattern"] = nil
	nulls["runtime_verification"] = map[string]any{"command": nil, "facts_from": nil}
	nulls["integration"] = map[string]any{"plugin": "none", "topology": nil, "receipts_required": []any{}}
	if err := ok(nulls); err != nil {
		t.Fatalf("every nullable null: %v", err)
	}
}

// Decode and encode round-trip both populated and null values unchanged.
func TestProjectProfileRoundTrip(t *testing.T) {
	nulls := profile()
	nulls["retention"].(map[string]any)["blob_payload_ttl"] = nil
	nulls["vocabulary"].(map[string]any)["task_id_pattern"] = nil
	nulls["runtime_verification"] = map[string]any{"command": nil, "facts_from": nil}
	nulls["integration"] = map[string]any{"plugin": "none", "topology": nil, "receipts_required": []any{}}
	for name, doc := range map[string]map[string]any{"populated": profile(), "nulls": nulls} {
		in, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		p, err := contract.Decode[types.ProjectProfile]("config/project-profile", in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out, err := contract.Encode("config/project-profile", p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var a, b any
		if err := json.Unmarshal(in, &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(out, &b); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s: round trip changed the document:\nin  %s\nout %s", name, in, out)
		}
	}
}
