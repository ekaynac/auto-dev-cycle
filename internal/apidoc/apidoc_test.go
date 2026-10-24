// SPDX-License-Identifier: Apache-2.0

package apidoc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
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

// parsed is the document as a consumer reads it, strictly (unknown fields
// refused); the embedded schemas stay generic.
type parsed struct {
	OpenAPI    string                          `yaml:"openapi"`
	Info       map[string]string               `yaml:"info"`
	Security   []map[string][]string           `yaml:"security"`
	Paths      map[string]map[string]operation `yaml:"paths"`
	Components struct {
		SecuritySchemes map[string]map[string]string `yaml:"securitySchemes"`
		Schemas         map[string]any               `yaml:"schemas"`
	} `yaml:"components"`
}

func parse(t *testing.T, b []byte) parsed {
	t.Helper()
	var d parsed
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

// Every action has exactly one command endpoint; besides them there is
// only the bootstrap, which takes no bearer.
func TestOneEndpointPerAction(t *testing.T) {
	actions := registry(t)
	b, err := Generate(actions, schemas.FS())
	if err != nil {
		t.Fatal(err)
	}
	d := parse(t, b)
	if b := d.Paths[BootstrapPath]["post"]; b.OperationID != "bootstrap" || b.Action != "" ||
		b.RequestBody.Content["application/json"].Schema.Ref != "#/components/schemas/api.bootstrap-request" ||
		b.Responses["200"].Content["application/json"].Schema.Ref != "#/components/schemas/api.bootstrap-response" {
		t.Errorf("bootstrap: %+v", b)
	}
	commands := 0
	for p := range d.Paths {
		if strings.HasPrefix(p, CommandPath("")) || p == BootstrapPath {
			commands++
		}
	}
	if commands != len(actions)+1 {
		t.Fatalf("%d command and bootstrap paths for %d actions", commands, len(actions))
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
		if op.RequestBody.Content["application/json"].Schema.Ref != "#/components/schemas/api.command-request" ||
			op.Responses["200"].Content["application/json"].Schema.Ref != "#/components/schemas/api.command-response" {
			t.Errorf("%s: bodies %+v %+v", name, op.RequestBody, op.Responses)
		}
	}
}

// The reads are described as the handlers serve them: the event stream,
// whoami, the resource path and each registered resource, its items its
// record schema, GET only, with no body and no action, and every other
// path a command.
func TestReadPaths(t *testing.T) {
	b, err := Generate(registry(t), schemas.FS())
	if err != nil {
		t.Fatal(err)
	}
	d := parse(t, b)
	want := map[string]string{api.EventsPath: "", api.WhoamiPath: "api.whoami", api.ReadPath: "api.read-list", api.TaskPath: "api.read-list"}
	for p, schema := range want {
		op, ok := d.Paths[p]["get"]
		if !ok || len(d.Paths[p]) != 1 || op.Action != "" || op.RequestBody != nil || op.Responses["401"].Description == "" {
			t.Errorf("%s: %+v", p, d.Paths[p])
			continue
		}
		ok200 := op.Responses["200"].Content
		if schema == "" {
			if _, sse := ok200["text/event-stream"]; !sse {
				t.Errorf("%s: %+v", p, ok200)
			}
		} else if ok200["application/json"].Schema == nil || ok200["application/json"].Schema.Ref != "#/components/schemas/"+schema {
			t.Errorf("%s: %+v", p, ok200)
		}
	}
	for name, res := range api.Registered() {
		op, ok := d.Paths["/v1/"+name]["get"]
		var list []any
		if ok && op.Responses["200"].Content["application/json"].Schema != nil {
			list = op.Responses["200"].Content["application/json"].Schema.AllOf
		}
		want := fmt.Sprint([]any{map[string]any{"$ref": "#/components/schemas/api.read-list"}, map[string]any{"properties": map[string]any{
			"resource": map[string]any{"const": name}, "items": map[string]any{"items": map[string]any{"$ref": "#/components/schemas/" + ComponentName(res.Schema)}}}}})
		if len(d.Paths["/v1/"+name]) != 1 || op.RequestBody != nil || op.Action != "" || fmt.Sprint(list) != want || d.Components.Schemas[ComponentName(res.Schema)] == nil {
			t.Errorf("/v1/%s: %+v", name, d.Paths["/v1/"+name])
		}
	}
	if _, ok := d.Paths[api.WhoamiPath]["get"].Responses["403"]; ok {
		t.Error("whoami refuses as a grant domain")
	}
	for p := range d.Paths {
		_, resource := api.Registered()[strings.TrimPrefix(p, "/v1/")]
		if _, read := want[p]; !read && !resource && !strings.HasPrefix(p, CommandPath("")) && p != BootstrapPath && p != api.BlobsPath && p != api.SandboxProbePath {
			t.Errorf("an unknown path %s", p)
		}
	}
}

// offline refuses every URL: a reference that leaves the document fails.
type offline struct{}

func (offline) Load(url string) (any, error) { return nil, fmt.Errorf("fetched %s", url) }

// embedded decodes the generated document's embedded schemas.
func embedded(t *testing.T) map[string]any {
	t.Helper()
	b, err := Generate(registry(t), schemas.FS())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Components.Schemas
}

// Each component is its schema file exactly (the YAML says the same JSON
// values), under the file's own $id.
func TestComponentsAreTheSchemaFiles(t *testing.T) {
	for name, s := range embedded(t) {
		file := strings.ReplaceAll(name, ".", "/") + ".json"
		src, err := fs.ReadFile(schemas.FS(), file)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var want any
		if err := json.Unmarshal(src, &want); err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var gotv any
		if err := json.Unmarshal(got, &gotv); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotv, want) {
			t.Errorf("%s differs from %s", name, file)
		}
	}
}

// The document is self-contained: a resolver that knows only its components
// (each under its own $id, a JSON Schema compound document) compiles every
// one, so every relative $ref, fragments included, resolves inside it.
func TestReferencesResolveOffline(t *testing.T) {
	comps := embedded(t)
	compiler := func(skip string) (*jsonschema.Compiler, map[string]string) {
		c := jsonschema.NewCompiler()
		c.UseLoader(offline{})
		ids := map[string]string{}
		for name, s := range comps {
			id, _ := s.(map[string]any)["$id"].(string)
			ids[name] = id
			if name == skip {
				continue
			}
			j, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			res, err := jsonschema.UnmarshalJSON(bytes.NewReader(j))
			if err != nil {
				t.Fatal(err)
			}
			if err := c.AddResource(id, res); err != nil {
				t.Fatal(err)
			}
		}
		return c, ids
	}
	c, ids := compiler("")
	for _, want := range []string{"api.command-request", "api.command-response", "records.approval", "common.defs"} {
		if ids[want] != schemas.Base+strings.ReplaceAll(want, ".", "/")+".json" {
			t.Errorf("component %s: $id %q", want, ids[want])
		}
	}
	for name, id := range ids {
		if _, err := c.Compile(id); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The control: with a referenced file left out, compiling fails.
	c, ids = compiler("common.defs")
	if _, err := c.Compile(ids["api.command-response"]); err == nil {
		t.Error("compiled with common/defs.json missing")
	}
}

func TestDeterministicAndDeprecated(t *testing.T) {
	actions := maps.Clone(registry(t))
	v := types.Semver("0.2.0")
	a := actions["note.add"]
	a.Deprecated = &v
	actions["note.add"] = a
	first, err := Generate(actions, schemas.FS())
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if again, _ := Generate(actions, schemas.FS()); !bytes.Equal(again, first) {
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
	if _, err := Generate(actions, schemas.FS()); err == nil || !strings.Contains(err.Error(), "note.remove") {
		t.Fatalf("got %v", err)
	}
}

// bundle follows every reference, inside arrays and fragments included,
// relative to the referring file.
func TestBundleFollowsEveryReference(t *testing.T) {
	got, err := bundle(fstest.MapFS{
		"a/root.json":   {Data: []byte(`{"oneOf": [{"$ref": "../b/x.json#/$defs/X"}], "items": {"$ref": "y.json"}, "not": {"$ref": "#/$defs/Z"}}`)},
		"b/x.json":      {Data: []byte(`{"$defs": {"X": {"$ref": "../a/y.json"}}}`)},
		"a/y.json":      {Data: []byte(`{}`)},
		"c/unused.json": {Data: []byte(`{}`)},
	}, "a/root.json")
	if err != nil {
		t.Fatal(err)
	}
	if names := slices.Sorted(maps.Keys(got)); !slices.Equal(names, []string{"a.root", "a.y", "b.x"}) {
		t.Fatalf("bundled %v", names)
	}
	if _, err := bundle(fstest.MapFS{"a.json": {Data: []byte(`{"$ref": "missing.json"}`)}}, "a.json"); err == nil {
		t.Error("a reference to a missing file was bundled")
	}
}
