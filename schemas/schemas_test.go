// SPDX-License-Identifier: Apache-2.0

package schemas

import (
	"encoding/json"
	"io/fs"
	"reflect"
	"testing"
	"testing/fstest"
)

// The expected value was computed outside Go:
//
//	printf 'a/x.json\0%s\nb/c/y.json\0%s\nc/r.yaml\0%s\n' "$(printf '{}' | sha256sum | cut -c1-64)" \
//	  "$(printf '[1]' | sha256sum | cut -c1-64)" "$(printf 'k: v\n' | sha256sum | cut -c1-64)" | sha256sum
func TestDigestOfMatchesTheFraming(t *testing.T) {
	got, err := DigestOf(fstest.MapFS{
		"b/c/y.json":   {Data: []byte("[1]")},
		"a/x.json":     {Data: []byte("{}")},
		"a/readme.md":  {Data: []byte("not a schema")},
		"c/r.yaml":     {Data: []byte("k: v\n")},
		"b/c/notes.go": {Data: []byte("package c")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "sha256:b51ba245776da7c1ff52e43d3a9a970ddd59054b26e4c124dbf14575279bb8e1"; got != want {
		t.Fatalf("DigestOf = %s, want %s", got, want)
	}
}

// The embedded inventory is exactly the schema files of this release; adding
// one means listing it here and in the go:embed directive.
func TestEmbeddedInventory(t *testing.T) {
	paths, err := Paths()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"adapter/describe-result.json", "adapter/event.json", "adapter/fenced-params.json", "adapter/outcome.json", "adapter/prepare-params.json", "adapter/run-params.json", "adapter/run-result.json", "adapter/tool-response-params.json", "api/bootstrap-request.json", "api/bootstrap-response.json", "api/command-request.json", "api/command-response.json", "api/grant.json", "api/note.json", "api/proposal.json", "api/read-list.json", "api/scope/actor-revoke.json", "api/scope/agent-enrol.json", "api/scope/grant-create.json", "api/scope/grant-revoke.json", "api/scope/note-add.json", "api/scope/proposal-withdraw.json", "api/scope/seat-release.json", "api/scope/seat-take.json", "api/scope/token-issue.json", "api/scope/token-replace.json", "api/scope/token-revoke.json", "api/seat.json", "api/whoami.json", "common/defs.json", "config/project-profile.json", "records/actor.json", "records/approval.json", "records/attempt-output-refusal.json", "records/attempt-record.json", "records/attempt.json", "records/bootstrap-renewed.json", "records/challenge.json", "records/effect-event.json", "records/grant-event.json", "records/grant.json", "records/identity-event.json", "records/instrument-contract.json", "records/lease-event.json", "records/lease.json", "records/manifest-delivery.json", "records/manifest-finalized.json", "records/manifest-intent.json", "records/note.json", "records/owed-input.json", "records/proposal.json", "records/receipt.json", "records/ref.json", "records/resource-group.json", "records/seat-event.json", "records/token-recovered.json", "records/token.json", "records/usage-measurement.json", "records/view-fingerprint.json", "testing/contract-fixture.json", "vocab/registry.json"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("embedded schemas = %v, want %v", paths, want)
	}
	vocab, err := VocabPaths()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"vocab/actions.yaml", "vocab/holds.yaml", "vocab/outcomes.yaml", "vocab/reasons.yaml"}; !reflect.DeepEqual(vocab, want) {
		t.Fatalf("embedded registries = %v, want %v", vocab, want)
	}
	a, _ := SetDigest()
	b, _ := DigestOf(FS())
	if a != b {
		t.Fatalf("SetDigest %s differs from DigestOf(FS()) %s", a, b)
	}
}

// Every file declares draft 2020-12 and an $id equal to Base + its path, so
// cross-file $refs resolve to the embedded bytes and nothing else.
func TestEveryFileIDMatchesItsPath(t *testing.T) {
	paths, err := Paths()
	if err != nil || len(paths) == 0 {
		t.Fatalf("no embedded schemas: %v", err)
	}
	for _, p := range paths {
		b, err := fs.ReadFile(FS(), p)
		if err != nil {
			t.Fatal(err)
		}
		var head struct {
			Schema string `json:"$schema"`
			ID     string `json:"$id"`
		}
		if err := json.Unmarshal(b, &head); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if head.Schema != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("%s: $schema = %q", p, head.Schema)
		}
		if head.ID != Base+p {
			t.Errorf("%s: $id = %q, want %q", p, head.ID, Base+p)
		}
	}
}
