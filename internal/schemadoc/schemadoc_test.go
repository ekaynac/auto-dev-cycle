// SPDX-License-Identifier: Apache-2.0

package schemadoc

import (
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ekaynac/auto-dev-cycle/schemas"
)

const thingSchema = `{"title": "Thing", "description": "A <thing>.", "type": "object", "additionalProperties": false,
	"required": ["zeta", "alpha"],
	"properties": {
		"zeta": {"description": "Last | first.", "enum": ["x", null]},
		"alpha": {"$ref": "../b/defs.json#/$defs/Id", "pattern": "^thg_"},
		"mid": {"type": "array", "uniqueItems": true, "minItems": 1, "maxItems": 9, "items": {"type": "integer", "minimum": 1, "maximum": 5}},
		"kind": {"const": "thing"},
		"none": {"const": []},
		"maybe": {"oneOf": [{"type": "null"}, {"$ref": "#/$defs/Local"}]},
		"payload": {"type": "object", "x-adc-open-payload": "kind"},
		"tags": {"type": "object", "minProperties": 1, "propertyNames": {"$ref": "#/$defs/Local"}, "additionalProperties": {"type": "integer"}},
		"empty": {"type": "object", "properties": {}},
		"deeper": {"$ref": "#/$defs/Local/not"},
		"bag": {"type": "object", "additionalProperties": {"type": "object", "properties": {"v": {"type": "boolean"}}}},
		"nest": {"description": "A nest.", "type": "object", "required": ["leaf"], "properties": {
			"leaf": {"type": "string", "format": "adc-time", "maxLength": 30},
			"deep": {"type": "array", "items": {"type": "object", "additionalProperties": false, "properties": {"n": {"type": "boolean"}}}}},
			"if": {"properties": {"leaf": {"const": "a"}}}, "then": {"required": ["deep"]}, "else": {"properties": {"deep": {"const": null}}}}
	},
	"allOf": [{"description": "Rule one.", "if": {}, "then": {}}, {"if": {}, "then": {}}],
	"$defs": {"Local": {"description": "Here.", "type": "string", "pattern": "^l$", "not": {"const": "ll"}}}}`

const thingPage = banner + "# Thing\n\n`schemas/a/thing.json`\n\nA &lt;thing&gt;.\n\n" +
	"## Fields of the document\n\n| Field | Required | Type | Description |\n|---|---|---|---|\n" +
	"| `zeta` | yes | one of `x`, `null` | Last \\| first. |\n" +
	"| `alpha` | yes | [Id](b-defs.md#id) (pattern `^thg_`) |  |\n" +
	"| `mid` |  | array of integer (minimum `1`, maximum `5`) (minItems `1`, maxItems `9`, unique items) |  |\n" +
	"| `kind` |  | `thing` |  |\n" +
	"| `none` |  | `[]` |  |\n" +
	"| `maybe` |  | null or [Local](#local) |  |\n" +
	"| `payload` |  | object (open payload) |  |\n" +
	"| `tags` |  | map of [Local](#local) to integer (minProperties `1`) |  |\n" +
	"| `empty` |  | object ([fields](#fields-of-empty)) |  |\n" +
	"| `deeper` |  | [Local/not](#local) |  |\n" +
	"| `bag` |  | map of string to object ([fields](#fields-of-bag)) |  |\n" +
	"| `nest` |  | object ([fields](#fields-of-nest)) | A nest. |\n\n" +
	"No other fields are allowed.\n\n" +
	"Conditional rules (2, `if`/`then`/`else` in the schema):\n\n" +
	"- Rule one. (when (see the schema), then (see the schema))\n" +
	"- when (see the schema), then (see the schema)\n\n" +
	"## Fields of empty\n\nNo fields are defined.\n\nOther fields are allowed.\n\n" +
	"## Fields of bag{}\n\n| Field | Required | Type | Description |\n|---|---|---|---|\n| `v` |  | boolean |  |\n\nOther fields are allowed.\n\n" +
	"## Fields of nest\n\n| Field | Required | Type | Description |\n|---|---|---|---|\n" +
	"| `leaf` | yes | string (format `adc-time`, maxLength `30`) |  |\n" +
	"| `deep` |  | array of object ([fields](#fields-of-nestdeep)) |  |\n\n" +
	"Other fields are allowed.\n\n" +
	"Conditional rules (1, `if`/`then`/`else` in the schema):\n\n" +
	"- when `leaf` `a`, then `deep` required; otherwise `deep` `null`\n\n" +
	"## Fields of nest.deep[]\n\n| Field | Required | Type | Description |\n|---|---|---|---|\n" +
	"| `n` |  | boolean |  |\n\n" +
	"No other fields are allowed.\n\n" +
	"## Definitions\n\n### Local\n\nHere.\n\nType: string (pattern `^l$`, not `ll`)\n"

func TestRender(t *testing.T) {
	fsys := fstest.MapFS{
		"a/thing.json": {Data: []byte(thingSchema)},
		"b/defs.json":  {Data: []byte(`{"oneOf": [{"$ref": "#/$defs/Id"}], "$defs": {"Id": {"type": "string"}}}`)},
	}
	pages, err := Render(fsys, []string{"a/thing.json", "b/defs.json"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 3 || pages[0].Name != "a-thing.md" || pages[1].Name != "b-defs.md" || pages[2].Name != "README.md" {
		t.Fatalf("%v", pages)
	}
	if pages[0].Content != thingPage {
		t.Errorf("page:\n%s\nwant:\n%s", pages[0].Content, thingPage)
	}
	if !strings.Contains(pages[1].Content, "A document is one of: [Id](#id).") {
		t.Errorf("root oneOf:\n%s", pages[1].Content)
	}
	if idx := pages[2].Content; !strings.Contains(idx, "| [`a/thing.json`](a-thing.md) | Thing |") || !strings.Contains(idx, "| [`b/defs.json`](b-defs.md) | b/defs.json |") {
		t.Errorf("index:\n%s", idx)
	}
}

// githubAnchor is GitHub's heading anchor, written independently of Slug:
// lowercase, punctuation other than - and _ dropped, spaces to hyphens.
func githubAnchor(h string) string {
	h = strings.ToLower(h)
	h = regexp.MustCompile(`[^\p{L}\p{N} _-]`).ReplaceAllString(h, "")
	return strings.ReplaceAll(h, " ", "-")
}

// Every link in the generated reference resolves to a generated page and
// one heading there, by GitHub's anchors; no linked anchor is ambiguous.
func TestLinksResolve(t *testing.T) {
	files, err := schemas.Paths()
	if err != nil {
		t.Fatal(err)
	}
	pages, err := Render(schemas.FS(), files)
	if err != nil {
		t.Fatal(err)
	}
	anchors := map[string]map[string]int{}
	heading := regexp.MustCompile(`(?m)^#+ (.+)$`)
	for _, p := range pages {
		anchors[p.Name] = map[string]int{}
		for _, m := range heading.FindAllStringSubmatch(p.Content, -1) {
			anchors[p.Name][githubAnchor(m[1])]++
		}
	}
	link := regexp.MustCompile(`\]\(([^)#]*)(?:#([^)]*))?\)`)
	code := regexp.MustCompile("`[^`]*`") // a pattern in a code span is not a link
	n := 0
	for _, p := range pages {
		for _, m := range link.FindAllStringSubmatch(code.ReplaceAllString(p.Content, ""), -1) {
			n++
			target := m[1]
			if target == "" {
				target = p.Name
			}
			a, ok := anchors[target]
			if !ok || (m[2] != "" && a[m[2]] != 1) {
				t.Errorf("%s: link %s resolves to %d headings", p.Name, m[0], a[m[2]])
			}
		}
	}
	if n < 200 {
		t.Fatalf("only %d links checked", n)
	}
}
