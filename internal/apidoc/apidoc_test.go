// SPDX-License-Identifier: Apache-2.0

package apidoc

import (
	"bytes"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
	"github.com/ekaynac/auto-dev-cycle/schemas"
)

func registry(t *testing.T) map[string]types.Action {
	t.Helper()
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg.Actions
}

func parse(t *testing.T, b []byte) document {
	t.Helper()
	var d document
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

// Every action has exactly one command endpoint, and nothing else is one.
func TestOneEndpointPerAction(t *testing.T) {
	actions := registry(t)
	b, err := Generate(actions)
	if err != nil {
		t.Fatal(err)
	}
	d := parse(t, b)
	if len(d.Paths) != len(actions) {
		t.Fatalf("%d paths for %d actions", len(d.Paths), len(actions))
	}
	for name, a := range actions {
		ops, ok := d.Paths[CommandPath(name)]
		if !ok || len(ops) != 1 {
			t.Errorf("%s: %v", name, ops)
			continue
		}
		op := ops["post"]
		if op.Action != name || op.OperationID != "command."+name || !slices.Equal(op.Tags, []string{string(a.Family)}) ||
			op.Deprecated != (a.Deprecated != nil) || !op.RequestBody.Required {
			t.Errorf("%s: %+v", name, op)
		}
		if op.RequestBody.Content["application/json"].Schema.Ref != "#/components/schemas/CommandRequest" ||
			op.Responses["200"].Content["application/json"].Schema.Ref != "#/components/schemas/CommandResponse" {
			t.Errorf("%s: bodies %+v %+v", name, op.RequestBody, op.Responses)
		}
	}
}

// The document's schema references name embedded schema files.
func TestReferencesResolve(t *testing.T) {
	b, err := Generate(registry(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, r := range parse(t, b).Components.Schemas {
		if _, err := fs.Stat(schemas.FS(), r.Ref); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestDeterministicAndDeprecated(t *testing.T) {
	actions := maps.Clone(registry(t))
	v := types.Semver("0.2.0")
	a := actions["note.add"]
	a.Deprecated = &v
	actions["note.add"] = a
	first, err := Generate(actions)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if again, _ := Generate(actions); !bytes.Equal(again, first) {
			t.Fatal("two generations differ")
		}
	}
	if !parse(t, first).Paths[CommandPath("note.add")]["post"].Deprecated {
		t.Error("a deprecated action is not marked")
	}
}

func TestMisregisteredAction(t *testing.T) {
	actions := maps.Clone(registry(t))
	actions["note.remove"] = actions["note.add"]
	if _, err := Generate(actions); err == nil || !strings.Contains(err.Error(), "note.remove") {
		t.Fatalf("got %v", err)
	}
}
