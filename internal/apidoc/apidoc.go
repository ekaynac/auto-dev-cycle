// SPDX-License-Identifier: Apache-2.0

// Package apidoc generates schemas/openapi.yaml, the single API description
// (docs/design/m0/08 "Transport"), from the action registry: one command
// endpoint per action, POST /v1/commands/{action}. The document is
// self-contained: every schema the bodies reach, transitively, is embedded
// under components with its own $id, so a consumer resolves each relative
// $ref inside the document (a JSON Schema compound document) and never
// fetches a schema URL. It also describes the reads (internal/api): the
// event stream, whoami and the resource path, whose resources join with
// their families.
package apidoc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
)

// CommandPath is the endpoint of an action.
func CommandPath(action string) string { return "/v1/commands/" + action }

// BootstrapPath is the one-time bootstrap's endpoint (04 "Bootstrap").
const BootstrapPath = "/v1/bootstrap"

// The schema files the command and bootstrap bodies are.
const (
	requestSchema     = "api/command-request.json"
	responseSchema    = "api/command-response.json"
	bootstrapRequest  = "api/bootstrap-request.json"
	bootstrapResponse = "api/bootstrap-response.json"
	readSchema        = "api/read-list.json"
	whoamiSchema      = "api/whoami.json"
	blobSchema        = "api/blob-upload.json"
	probeSchema       = "api/sandbox-probe.json"
)

// ComponentName is the component a schema file is embedded under
// ("api/command-request.json" → "api.command-request").
func ComponentName(file string) string {
	return strings.ReplaceAll(strings.TrimSuffix(file, ".json"), "/", ".")
}

type ref struct {
	Ref   string `yaml:"$ref,omitempty"`
	AllOf []any  `yaml:"allOf,omitempty"` // a registered resource's read: the list, its items one schema
}

type media struct {
	Schema *ref `yaml:"schema,omitempty"`
}

type body struct {
	Description string           `yaml:"description,omitempty"`
	Required    bool             `yaml:"required,omitempty"`
	Content     map[string]media `yaml:"content,omitempty"`
}

type operation struct {
	OperationID string                 `yaml:"operationId"`
	Tags        []string               `yaml:"tags"`
	Summary     string                 `yaml:"summary"`
	Deprecated  bool                   `yaml:"deprecated,omitempty"`
	Action      string                 `yaml:"x-adc-action,omitempty"`
	Security    *[]map[string][]string `yaml:"security,omitempty"` // nil: the document's
	Parameters  []parameter            `yaml:"parameters,omitempty"`
	RequestBody *body                  `yaml:"requestBody,omitempty"`
	Responses   map[string]body        `yaml:"responses"`
}

type parameter struct {
	Name        string         `yaml:"name"`
	In          string         `yaml:"in"`
	Required    bool           `yaml:"required,omitempty"`
	Description string         `yaml:"description"`
	Schema      map[string]any `yaml:"schema"`
}

type components struct {
	SecuritySchemes map[string]map[string]string `yaml:"securitySchemes"`
	Schemas         map[string]*yaml.Node        `yaml:"schemas"`
}

type document struct {
	OpenAPI    string                          `yaml:"openapi"`
	Info       map[string]string               `yaml:"info"`
	Security   []map[string][]string           `yaml:"security"`
	Paths      map[string]map[string]operation `yaml:"paths"`
	Components components                      `yaml:"components"`
}

func jsonOf(file string) map[string]media {
	return map[string]media{"application/json": {Schema: &ref{Ref: "#/components/schemas/" + ComponentName(file)}}}
}

// readResponses are a read's responses: ok, the refusals every read can
// give (403 and 503 carry a result: outcome, reason, detail) and extra
// (status, description pairs).
func readResponses(ok body, extra ...string) map[string]body {
	out := map[string]body{"200": ok,
		"400": {Description: "A query parameter the read does not take, or a malformed one."},
		"401": {Description: "No valid bearer token on this listener."},
		"503": {Description: "could_not_run/store_unreadable, or too many authentications in progress (Retry-After)."}}
	for i := 0; i+1 < len(extra); i += 2 {
		out[extra[i]] = body{Description: extra[i+1]}
	}
	return out
}

const notGranted = "failed/action_not_granted: no grant in force grants reads."

// reads are the read paths (08 "Reads, commands, events").
func reads() map[string]map[string]operation {
	str := map[string]any{"type": "string"}
	op := func(id, summary string, params []parameter, responses map[string]body) map[string]operation {
		return map[string]operation{"get": {OperationID: "read." + id, Tags: []string{"read"}, Summary: summary, Parameters: params, Responses: responses}}
	}
	out := map[string]map[string]operation{
		api.EventsPath: op("events", "The event stream, resumable by seq and filtered server-side by the caller's read scope.", []parameter{
			{Name: "after", In: "query", Description: "Resume after this seq.", Schema: map[string]any{"type": "integer", "minimum": 0}},
			{Name: "kinds", In: "query", Description: "Comma-separated kinds or kind names; narrows, never widens.", Schema: str},
			{Name: "project", In: "query", Description: "One project; narrows, never widens.", Schema: str}},
			readResponses(body{Description: "Server-Sent Events: event frames (id: seq) the caller's read scope covers, each payload cut to its kind's allow-list or withheld, then a closed frame with the result that ended the stream.",
				Content: map[string]media{"text/event-stream": {}}}, "403", notGranted)),
		api.WhoamiPath: op("whoami", "The authenticated caller and its effective read scope; no grant domain.", nil,
			readResponses(body{Description: "The caller.", Content: jsonOf(whoamiSchema)})),
		api.ReadPath: op("resource", "The records of one resource the caller's read scope covers, each authorized in one snapshot.",
			[]parameter{{Name: "resource", In: "path", Required: true, Description: "A registered resource.", Schema: str}},
			readResponses(body{Description: "The readable records.", Content: jsonOf(readSchema)}, "403", notGranted, "404", "No such resource.")),
		api.TaskPath: op("task", "One task's record, its benchmark facts included (P25): the tasks list holding it alone, or empty when the caller reads no such task.",
			[]parameter{{Name: "task", In: "path", Required: true, Description: "A task id (tsk_...).", Schema: str},
				{Name: "without", In: "query", Description: "bench: the record without its benchmark facts (null), as a poll reads it.", Schema: map[string]any{"enum": []string{"bench"}}}},
			readResponses(body{Description: "The task's record, if the caller reads it.", Content: jsonOf(readSchema)}, "400", "Not a task id, or another query.", "403", notGranted)),
	}
	out[api.BlobsPath] = map[string]operation{
		"post": {OperationID: "blob.upload", Tags: []string{"blob"}, Summary: "Store the body (at most 1 MiB) as the caller's upload, for a later command of the same caller to reference before it expires; an operator's or a supervisor's.",
			RequestBody: &body{Required: true, Content: map[string]media{"application/octet-stream": {}}},
			Responses: readResponses(body{Description: "The upload.", Content: jsonOf(blobSchema)}, "403", "failed/actor_kind_refused: not an operator or a supervisor.",
				"413", "failed/upload_bound_exceeded: over 1 MiB, or over the caller's 64 uploads or 32 MiB; nothing was stored.")},
		"get": op("blob-uploads", "The unclaimed uploads, never their bytes; an operator's.", nil,
			readResponses(body{Description: "The uploads.", Content: jsonOf(readSchema)}, "403", "failed/operator_only: not an operator."))["get"],
	}
	mode := map[string]any{"enum": []string{"gate", "build"}}
	out[api.SandboxProbePath] = map[string]operation{"post": {OperationID: "dev.sandbox_probe", Tags: []string{"dev"},
		Summary: "Run the fixed sandbox probe in a fresh run of a pinned image, with no task or lease; an operator's, one at a time.",
		Parameters: []parameter{{Name: "image", In: "query", Required: true, Description: "A fully qualified reference pinned by digest.", Schema: str},
			{Name: "mode", In: "query", Required: true, Description: "gate (nothing reachable) or build (the gateway socket only).", Schema: mode}},
		Responses: readResponses(body{Description: "What the probe saw; an empty violations list is a pass.", Content: jsonOf(probeSchema)},
			"403", "failed/operator_only: not an operator.", "503", "could_not_run/precondition_failed: no sandbox configured, the image not at its digest, or the probe incomplete; or busy (Retry-After).")}}
	for name, res := range api.Registered() {
		list := &ref{AllOf: []any{map[string]string{"$ref": "#/components/schemas/" + ComponentName(readSchema)},
			map[string]any{"properties": map[string]any{"resource": map[string]string{"const": name},
				"items": map[string]any{"items": map[string]string{"$ref": "#/components/schemas/" + ComponentName(res.Schema)}}}}}}
		out["/v1/"+name] = op(name, fmt.Sprintf("The %s records the caller's read scope covers, each authorized in one snapshot.", res.Domain), nil,
			readResponses(body{Description: "The readable records.", Content: map[string]media{"application/json": {Schema: list}}}, "403", notGranted))
	}
	return out
}

// readSchemas are the schema files of the reads: the list, whoami and each
// registered resource's record.
func readSchemas() []string {
	out := []string{readSchema, whoamiSchema, blobSchema, probeSchema}
	for _, res := range api.Registered() {
		out = append(out, res.Schema)
	}
	return out
}

// Generate renders the document for the registry's actions over the schema
// tree (schemas.FS()); the same inputs always give the same bytes.
func Generate(actions map[string]types.Action, schemaFS fs.FS) ([]byte, error) {
	embedded, err := bundle(schemaFS, append([]string{requestSchema, responseSchema, bootstrapRequest, bootstrapResponse}, readSchemas()...)...)
	if err != nil {
		return nil, err
	}
	d := document{
		OpenAPI: "3.1.0",
		Info: map[string]string{"title": "auto-dev-cycle controller API", "version": "v1",
			"description": "Generated by tools/apigen from schemas/vocab/actions.yaml and schemas/ (make gen); do not edit."},
		Security: []map[string][]string{{"bearer": {}}},
		Paths:    reads(),
		Components: components{
			SecuritySchemes: map[string]map[string]string{"bearer": {"type": "http", "scheme": "bearer"}},
			Schemas:         embedded,
		},
	}
	for _, name := range slices.Sorted(maps.Keys(actions)) {
		a := actions[name]
		if string(a.Action) != name {
			return nil, fmt.Errorf("apidoc: action %q registered as %q", a.Action, name)
		}
		d.Paths[CommandPath(name)] = map[string]operation{"post": {
			OperationID: "command." + name,
			Tags:        []string{string(a.Family)},
			Summary:     a.Description,
			Deprecated:  a.Deprecated != nil,
			Action:      name,
			RequestBody: &body{Required: true, Content: jsonOf(requestSchema)},
			Responses: map[string]body{
				"200": {Description: "The command was decided: applied, proposed or refused.", Content: jsonOf(responseSchema)},
				"400": {Description: "Not a valid CommandRequest, or a scope or input the action refuses; nothing was decided."},
				"401": {Description: "No valid bearer token; nothing was decided."},
				"413": {Description: "The request body is too large; nothing was decided."},
				"503": {Description: "Too many authentications in progress (Retry-After); nothing was decided."},
			},
		}}
	}
	d.Paths[BootstrapPath] = map[string]operation{"post": {
		OperationID: "bootstrap",
		Tags:        []string{"admin"},
		Summary:     "Create the first operator and its first token with the one-time setup secret; served only on the private socket, without a bearer.",
		Security:    &[]map[string][]string{},
		RequestBody: &body{Required: true, Content: jsonOf(bootstrapRequest)},
		Responses: map[string]body{
			"200": {Description: "The bootstrap was decided: applied or refused.", Content: jsonOf(bootstrapResponse)},
			"403": {Description: "Not the private socket; nothing was decided."},
		},
	}}
	var buf bytes.Buffer
	buf.WriteString("# SPDX-License-Identifier: Apache-2.0\n# Generated by tools/apigen (make gen); do not edit.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(d); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// bundle reads the root schema files and every file they reference,
// transitively, each as a YAML node (JSON is YAML, and the node keeps the
// file's key order and number spellings) keyed by its component name.
func bundle(fsys fs.FS, roots ...string) (map[string]*yaml.Node, error) {
	out := map[string]*yaml.Node{}
	queue := slices.Clone(roots)
	for len(queue) > 0 {
		file := queue[0]
		queue = queue[1:]
		if _, done := out[ComponentName(file)]; done {
			continue
		}
		b, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, fmt.Errorf("apidoc: %w", err)
		}
		var doc any
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("apidoc: %s: %w", file, err)
		}
		for _, r := range refs(doc) {
			if target, _, _ := strings.Cut(r, "#"); target != "" {
				queue = append(queue, path.Join(path.Dir(file), target))
			}
		}
		var n yaml.Node
		if err := yaml.Unmarshal(b, &n); err != nil {
			return nil, fmt.Errorf("apidoc: %s: %w", file, err)
		}
		blockStyle(n.Content[0])
		out[ComponentName(file)] = n.Content[0]
	}
	return out, nil
}

// blockStyle drops the JSON (flow, double-quoted) styles, so the encoder
// writes block YAML and quotes a scalar only where its tag needs it.
func blockStyle(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		blockStyle(c)
	}
}

// refs lists every $ref value in a decoded schema.
func refs(v any) []string {
	var out []string
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			if s, ok := e.(string); ok && k == "$ref" {
				out = append(out, s)
			} else {
				out = append(out, refs(e)...)
			}
		}
	case []any:
		for _, e := range v {
			out = append(out, refs(e)...)
		}
	}
	return out
}
