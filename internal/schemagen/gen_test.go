// SPDX-License-Identifier: Apache-2.0

package schemagen

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// one wraps a $defs body into a single schema file.
func one(defs string) map[string][]byte {
	return map[string][]byte{"records/x.json": []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$defs":{` + defs + `}}`)}
}

const (
	envelope = `"E":{"type":"object","required":["a"],"properties":{"a":{"type":"string"}}}`
	// dispatch builds a struct with an if/then dispatch entry.
	dispatchHead = `"S":{"type":"object","additionalProperties":false,"required":["k","p"],"properties":{"k":{"enum":["x","y"]},"p":{"type":"object","x-adc-open-payload":"t"}},"allOf":[`
	scopeX       = `"X":{"type":"object","additionalProperties":false,"properties":{}}`
)

// condHead opens a struct for condition cases; the case closes it.
const condHead = `"C":{"type":"object","additionalProperties":false,"properties":{}},"A":{"type":"object","additionalProperties":false,"required":["o","d"],"properties":{"o":{"enum":["x","y"]},"d":{"type":"string"}},`

func TestRefusesShapesOutsideTheGrammar(t *testing.T) {
	cases := []struct{ name, defs, want string }{
		{"unbounded integer", `"A":{"type":"integer","minimum":0}`, "integer needs both minimum and maximum"},
		{"integer beyond 2^53-1", `"A":{"type":"integer","minimum":0,"maximum":9007199254740992}`, "within ±(2^53-1)"},
		{"number", `"A":{"type":"number"}`, `type "number"`},
		{"open object", `"A":{"type":"object"}`, "open object without x-adc-open-payload"},
		{"optional nullable", `"A":{"type":"object","additionalProperties":false,"properties":{"p":{"oneOf":[{"type":"null"},{"type":"string"}]}}}`, "nullable field must be required"},
		{"optional nullable via $ref", `"N":{"oneOf":[{"type":"null"},{"type":"string"}]},"A":{"type":"object","additionalProperties":false,"properties":{"p":{"$ref":"#/$defs/N"}}}`, "nullable field must be required"},
		{"nested nullable", `"A":{"oneOf":[{"type":"null"},{"oneOf":[{"type":"null"},{"type":"string"}]}]}`, "nested nullable"},
		{"undiscriminated oneOf", `"A":{"oneOf":[{"type":"object","additionalProperties":false,"required":["a","b"],"properties":{"a":{"type":"string"},"b":{"type":"string"}}},{"type":"object","additionalProperties":false,"required":["a"],"properties":{"a":{"type":"string"}}}]}`, "neither nullable"},
		{"oneOf of scalars", `"A":{"oneOf":[{"type":"string"},{"type":"boolean"}]}`, "union branches must be closed objects"},
		{"envelope as a field", envelope + `,"A":{"type":"object","additionalProperties":false,"properties":{"e":{"$ref":"#/$defs/E"}}}`, "may only be used as an allOf base"},
		{"allOf of a closed object", `"C":{"type":"object","additionalProperties":false,"properties":{"a":{"type":"string"}}},"A":{"type":"object","allOf":[{"$ref":"#/$defs/C"}],"unevaluatedProperties":false}`, "must name an envelope"},
		{"dangling $ref", `"A":{"$ref":"#/$defs/Missing"}`, "does not name"},
		{"no type", `"A":{"description":"x"}`, "no type"},

		// C2-1: if/then dispatch.
		{"if without required", scopeX + "," + dispatchHead + `{"if":{"properties":{"k":{"const":"x"}}},"then":{"properties":{"p":{"$ref":"#/$defs/X"}}}}]}`, `the if must have "required": ["k"]`},
		{"if requiring another property", scopeX + "," + dispatchHead + `{"if":{"properties":{"k":{"const":"x"}},"required":["p"]},"then":{"properties":{"p":{"$ref":"#/$defs/X"}}}}]}`, `the if must have "required": ["k"]`},
		{"else", scopeX + "," + dispatchHead + `{"if":{"properties":{"k":{"const":"x"}},"required":["k"]},"then":{"properties":{"p":{"$ref":"#/$defs/X"}}},"else":{}}]}`, `keyword "else"`},
		{"optional dispatch property", scopeX + `,"S":{"type":"object","additionalProperties":false,"required":["p"],"properties":{"k":{"enum":["x"]},"p":{"type":"object","x-adc-open-payload":"t"}},"allOf":[{"if":{"properties":{"k":{"const":"x"}},"required":["k"]},"then":{"properties":{"p":{"$ref":"#/$defs/X"}}}}]}`, "must be a required property"},
		{"dispatch value outside the enum", scopeX + "," + dispatchHead + `{"if":{"properties":{"k":{"const":"z"}},"required":["k"]},"then":{"properties":{"p":{"$ref":"#/$defs/X"}}}}]}`, `"z" is not a value of k`},

		// C2-2: an envelope struct closes only by unevaluatedProperties.
		{"envelope struct with additionalProperties", envelope + `,"A":{"type":"object","allOf":[{"$ref":"#/$defs/E"}],"additionalProperties":false,"properties":{"b":{"type":"string"}}}`, "closed by unevaluatedProperties:false alone"},
		{"envelope struct with both", envelope + `,"A":{"type":"object","allOf":[{"$ref":"#/$defs/E"}],"additionalProperties":false,"unevaluatedProperties":false}`, "closed by unevaluatedProperties:false alone"},
		{"unevaluatedProperties without an envelope", `"A":{"type":"object","additionalProperties":false,"unevaluatedProperties":false,"allOf":[],"properties":{}}`, "unevaluatedProperties is supported only"},
		{"property redeclaring an envelope property", envelope + `,"A":{"type":"object","allOf":[{"$ref":"#/$defs/E"}],"unevaluatedProperties":false,"properties":{"a":{"type":"string"}}}`, "already declared by the embedded envelope"},

		// C2-3: optional arrays and maps.

		// C2-4: generated name collisions.
		{"nested name collides with a $defs entry", `"AB":{"type":"string"},"A":{"type":"object","additionalProperties":false,"required":["b"],"properties":{"b":{"type":"object","additionalProperties":false,"properties":{}}}}`, "generated name AB collides"},
		{"enum constant collides with a type", `"KindA":{"type":"string"},"Kind":{"enum":["a"]}`, "generated name KindA collides"},
		{"enum value without a name", `"Kind":{"enum":["*"]}`, `enum value "*" gives no Go constant name`},
		{"field names collide", `"A":{"type":"object","additionalProperties":false,"required":["foo_bar","fooBar"],"properties":{"foo_bar":{"type":"string"},"fooBar":{"type":"string"}}}`, "both give the Go field name FooBar"},
		{"property without a field name", `"A":{"type":"object","additionalProperties":false,"properties":{"_":{"type":"string"}}}`, "gives no exported Go field name"},
		{"literal named list", `"A":{"oneOf":[{"const":"list"},{"type":"array","items":{"type":"string"}}]}`, "no usable Go field name next to List"},
		{"unexported $defs name", `"a":{"type":"string"}`, "not an exported Go identifier"},

		// C2-5: keywords that change the shape are refused, not ignored.
		{"patternProperties", `"A":{"type":"object","additionalProperties":false,"patternProperties":{"^x":{"type":"string"}}}`, `keyword "patternProperties"`},
		{"anyOf next to type", `"A":{"type":"string","anyOf":[{"minLength":1}]}`, `keyword "anyOf"`},
		{"not", `"A":{"type":"string","not":{"const":"x"}}`, `keyword "not"`},
		{"dependentSchemas", `"A":{"type":"object","additionalProperties":false,"properties":{},"dependentSchemas":{}}`, `keyword "dependentSchemas"`},
		// Astra #19: keyed branches are exactly the single-key shape.
		{"keyed branch embedding an envelope", envelope + `,"K":{"oneOf":[{"type":"object","allOf":[{"$ref":"#/$defs/E"}],"unevaluatedProperties":false,"required":["x"],"properties":{"x":{"type":"string"}}},{"type":"object","additionalProperties":false,"required":["y"],"properties":{"y":{"type":"string"}}}]}`, "neither nullable"},
		{"keyed branch with a condition", `"K":{"oneOf":[{"type":"object","additionalProperties":false,"required":["x"],"properties":{"x":{"type":"string"}},"if":{"properties":{"q":{"const":"a"}}},"then":{"required":["q"]}},{"type":"object","additionalProperties":false,"required":["y"],"properties":{"y":{"type":"string"}}}]}`, "neither nullable"},
		// Astra #19: nullability through alias chains.
		{"optional nullable via an alias", `"N":{"oneOf":[{"type":"null"},{"type":"string"}]},"Al":{"$ref":"#/$defs/N"},"A":{"type":"object","additionalProperties":false,"properties":{"p":{"$ref":"#/$defs/Al"}}}`, "nullable field must be required"},
		{"nested nullable via an alias", `"N":{"oneOf":[{"type":"null"},{"type":"string"}]},"Al":{"$ref":"#/$defs/N"},"A":{"oneOf":[{"type":"null"},{"$ref":"#/$defs/Al"}]}`, "nested nullable"},
		{"nullable keyed branch via an alias", `"N":{"oneOf":[{"type":"null"},{"type":"string"}]},"Al":{"$ref":"#/$defs/N"},"K":{"oneOf":[{"type":"object","additionalProperties":false,"required":["x"],"properties":{"x":{"$ref":"#/$defs/Al"}}},{"type":"object","additionalProperties":false,"required":["y"],"properties":{"y":{"type":"string"}}}]}`, "may not be nullable"},
		// Astra #19: JSON keys a struct tag cannot carry.
		{"key with a comma", `"A":{"type":"object","additionalProperties":false,"required":["a,b"],"properties":{"a,b":{"type":"string"}}}`, "cannot be written as a Go JSON tag"},
		{"key with a quote", `"A":{"type":"object","additionalProperties":false,"required":["a\"b"],"properties":{"a\"b":{"type":"string"}}}`, "cannot be written as a Go JSON tag"},
		{"key that is a dash", `"A":{"type":"object","additionalProperties":false,"properties":{"-":{"type":"string"}}}`, "cannot be written as a Go JSON tag"},
		// Astra #19: every schema-bearing position is checked.
		{"union branch $ref with a sibling", `"C":{"type":"object","additionalProperties":false,"required":["t"],"properties":{"t":{"const":"c"}}},"D":{"type":"object","additionalProperties":false,"required":["t"],"properties":{"t":{"const":"d"}}},"A":{"oneOf":[{"$ref":"#/$defs/C","anyOf":[]},{"$ref":"#/$defs/D"}]}`, "may not have sibling keywords"},
		{"condition with a deep not", condHead + `"allOf":[{"if":{"properties":{"o":{"const":"x"}}},"then":{"properties":{"d":{"not":{"not":{"anyOf":[]}}}}}}]}`, `keyword "anyOf" is not supported on a constraint`},
		{"propertyNames with an unknown keyword", `"A":{"type":"object","additionalProperties":{"type":"string"},"propertyNames":{"anyOf":[]}}`, `keyword "anyOf" is not supported on a key names`},
		{"propertyNames not a string", `"A":{"type":"object","additionalProperties":{"type":"string"},"propertyNames":{"type":"integer"}}`, "propertyNames must be a string schema"},
		{"propertyNames $ref to an integer", `"I":{"type":"integer","minimum":0,"maximum":9},"A":{"type":"object","additionalProperties":{"type":"string"},"propertyNames":{"$ref":"#/$defs/I"}}`, "propertyNames must be a string schema"},
		// Astra #19: generated methods are reserved.
		{"literal naming a generated method", `"A":{"oneOf":[{"const":"marshalJSON"},{"type":"array","items":{"type":"string"}}]}`, "next to List and the generated methods"},

		// Refinements: string keywords beside a $ref to a plain string schema.
		{"refinement of an integer", `"B":{"type":"integer","minimum":0,"maximum":9},"A":{"$ref":"#/$defs/B","pattern":"x"}`, "must name a string schema"},
		{"refinement of an enum", `"B":{"enum":["x"]},"A":{"$ref":"#/$defs/B","minLength":1}`, "must name a string schema"},
		{"refinement changing the type", `"B":{"type":"string"},"A":{"$ref":"#/$defs/B","type":"string"}`, `keyword "type" is not supported on a refinement`},

		// Validation-only conditions.
		{"condition on an undeclared property", condHead + `"allOf":[{"if":{"properties":{"o":{"const":"x"}}},"then":{"properties":{"q":{"minLength":1}}}}]}`, `"q" is not a property`},
		{"condition requiring an undeclared property", condHead + `"allOf":[{"if":{"properties":{"o":{"const":"x"}}},"then":{"required":["q"]}}]}`, `required names "q"`},
		{"condition changing a type", condHead + `"allOf":[{"if":{"properties":{"o":{"const":"x"}}},"then":{"properties":{"d":{"type":"integer"}}}}]}`, `keyword "type" is not supported on a constraint`},
		{"condition selecting a $ref in else", condHead + `"allOf":[{"if":{"properties":{"o":{"const":"x"}}},"then":{"properties":{"d":{"minLength":1}}},"else":{"properties":{"d":{"$ref":"#/$defs/C"}}}}]}`, `keyword "$ref" is not supported on a constraint`},
		{"condition without then", condHead + `"if":{"properties":{"o":{"const":"x"}}}}`, "needs both if and then"},
		{"condition with a nested schema keyword", condHead + `"allOf":[{"if":{"properties":{"o":{"const":"x"}}},"then":{"properties":{"d":{"not":{"oneOf":[]}}}}}]}`, `keyword "oneOf" is not supported on a constraint`},
		{"prefixItems", `"A":{"type":"array","items":{"type":"string"},"prefixItems":[]}`, `keyword "prefixItems"`},
		{"nested $defs", `"A":{"type":"string","$defs":{}}`, `keyword "$defs"`},
		{"required not in properties", `"A":{"type":"object","additionalProperties":false,"required":["x"],"properties":{}}`, `required names "x"`},
		{"nullable keyed-union branch", `"A":{"oneOf":[{"type":"object","additionalProperties":false,"required":["a"],"properties":{"a":{"oneOf":[{"type":"null"},{"type":"string"}]}}},{"type":"object","additionalProperties":false,"required":["b"],"properties":{"b":{"type":"string"}}}]}`, "keyed-union branch may not be nullable"},
		{"inline keyed branch with an unknown keyword", `"A":{"oneOf":[{"type":"object","additionalProperties":false,"required":["a"],"properties":{"a":{"type":"string"}},"minProperties":1},{"type":"object","additionalProperties":false,"required":["b"],"properties":{"b":{"type":"string"}}}]}`, `keyword "minProperties"`},
		{"integer enum", `"A":{"type":"integer","minimum":0,"maximum":2,"enum":[1,2]}`, "enum may only be of type string"},
		{"bare null", `"A":{"type":"null"}`, "null is supported only as a branch"},
		{"boolean schema", `"A":{"type":"array","items":true}`, "a schema must be an object"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Generate(one(c.defs), "types")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
			if !strings.Contains(err.Error(), "records/x.json") {
				t.Errorf("error does not name the file: %v", err)
			}
		})
	}
}

// files builds a schema set from path → $defs body.
func files(defs map[string]string) map[string][]byte {
	out := map[string][]byte{}
	for p, d := range defs {
		out[p] = []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$defs":{` + d + `}}`)
	}
	return out
}

func TestRefusesAcrossFiles(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"optional nullable from another file", map[string]string{
			"common/x.json":  `"N":{"oneOf":[{"type":"null"},{"type":"string"}]}`,
			"records/y.json": `"A":{"type":"object","additionalProperties":false,"properties":{"p":{"$ref":"../common/x.json#/$defs/N"}}}`,
		}, "nullable field must be required"},
		{"refinement through a cross-file chain to an integer", map[string]string{
			"common/x.json":  `"I":{"type":"integer","minimum":0,"maximum":9},"Al":{"$ref":"#/$defs/I"}`,
			"records/y.json": `"R":{"$ref":"../common/x.json#/$defs/Al","pattern":"x"}`,
		}, "must name a string schema"},
		{"refinement of a dangling $ref", map[string]string{
			"records/y.json": `"R":{"$ref":"#/$defs/Missing","pattern":"x"}`,
		}, "must name a string schema"},
		{"two schema files with one output file", map[string]string{
			"a/b.json": `"P":{"type":"string"}`,
			"a_b.json": `"Q":{"type":"string"}`,
		}, "output file a_b.gen.go"},
		{"a schema file named like the helpers", map[string]string{
			"helpers.json": `"P":{"type":"string"}`,
		}, "output file helpers.gen.go"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Generate(files(c.files), "types")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

// A keyed branch from another file keeps resolving its own $refs there.
func TestCrossFileKeyedBranchResolvesInItsFile(t *testing.T) {
	out, err := Generate(files(map[string]string{
		"common/x.json": `"S":{"type":"string"},"B":{"type":"object","additionalProperties":false,"required":["b"],"properties":{"b":{"$ref":"#/$defs/S"}}}`,
		"records/y.json": `"K":{"oneOf":[{"$ref":"../common/x.json#/$defs/B"},` +
			`{"type":"object","additionalProperties":false,"required":["c"],"properties":{"c":{"type":"string"}}}]}`,
	}), "types")
	if err != nil {
		t.Fatal(err)
	}
	if src := string(out["records_y.gen.go"]); !strings.Contains(src, "B *S") {
		t.Fatalf("keyed field B is not typed by its own file's S:\n%s", src)
	}
}

func TestRefusesUnknownKeywordsAtTheRoot(t *testing.T) {
	files := map[string][]byte{"common/x.json": []byte(`{"$defs":{},"properties":{}}`)}
	if _, err := Generate(files, "types"); err == nil || !strings.Contains(err.Error(), `keyword "properties"`) {
		t.Fatalf("root keyword not refused: %v", err)
	}
}

func TestRefusesNameCollisions(t *testing.T) {
	files := one(`"Digest":{"type":"string"}`)
	files["common/y.json"] = []byte(`{"$defs":{"Digest":{"type":"string"}}}`)
	if _, err := Generate(files, "types"); err == nil || !strings.Contains(err.Error(), "type name Digest is used by both") {
		t.Fatalf("collision not refused: %v", err)
	}
}

// A null branch may carry a description: annotations never change the shape.
func TestNullBranchMayCarryAnnotations(t *testing.T) {
	_, err := Generate(one(`"A":{"type":"object","additionalProperties":false,"required":["p"],"properties":{"p":{"oneOf":[{"type":"null","description":"unset"},{"type":"string"}]}}}`), "types")
	if err != nil {
		t.Fatal(err)
	}
}

// supported exercises every shape of the grammar.
const supported = `
	"Kind":{"enum":["plan.approve","could_not_run"]},
	"Map":{"type":"object","additionalProperties":{"$ref":"#/$defs/Kind"}},
	"Opt":{"type":"object","additionalProperties":false,"required":["z","n","l","m","nul"],"properties":{
		"z":{"type":"string"},
		"n":{"type":"integer","minimum":0,"maximum":10},
		"b":{"type":"boolean"},
		"l":{"type":"array","items":{"type":"string"}},
		"m":{"$ref":"#/$defs/Map"},
		"nul":{"oneOf":[{"type":"null"},{"type":"string"}]},
		"raw":{"type":"object","x-adc-open-payload":"test"},
		"ol":{"type":"array","items":{"type":"string"}},
		"om":{"$ref":"#/$defs/Map"}}},
	"Set":{"oneOf":[{"const":"*"},{"type":"array","minItems":1,"items":{"type":"string"}}]},
	"Env":{"type":"object","required":["a"],"properties":{"a":{"type":"string"}}},
	"Evt":{"oneOf":[
		{"type":"object","allOf":[{"$ref":"#/$defs/Env"}],"unevaluatedProperties":false,"required":["t"],"properties":{"t":{"const":"one"}}},
		{"type":"object","allOf":[{"$ref":"#/$defs/Env"}],"unevaluatedProperties":false,"required":["t","n"],"properties":{"t":{"const":"two"},"n":{"type":"integer","minimum":0,"maximum":5}}}]},
	"Key":{"oneOf":[
		{"type":"object","additionalProperties":false,"required":["v"],"properties":{"v":{"type":"integer","minimum":0,"maximum":9}}},
		{"type":"object","additionalProperties":false,"required":["d"],"properties":{"d":{"type":"string"}}}]},
	"ScopeX":{"type":"object","additionalProperties":false,"required":["r"],"properties":{"r":{"type":"integer","minimum":1,"maximum":9}}},
	"Str":{"type":"string"},
	"Ref":{"$ref":"#/$defs/Str","pattern":"^a","minLength":1},
	"Cond":{"type":"object","additionalProperties":false,"required":["o","d","v"],
		"properties":{"o":{"enum":["x","y"]},"d":{"type":"string"},"v":{"oneOf":[{"type":"null"},{"enum":["go","no_go"]}]},"r":{"$ref":"#/$defs/Ref"}},
		"if":{"properties":{"o":{"not":{"const":"x"}}}},"then":{"properties":{"d":{"minLength":1}}},
		"allOf":[{"if":{"properties":{"o":{"const":"y"}},"required":["o"]},"then":{"properties":{"v":{"enum":["go","no_go"]}}},"else":{"properties":{"v":{"const":null}}}}]},
	"Disp":{"type":"object","additionalProperties":false,"required":["k","p"],
		"properties":{"k":{"enum":["x","y"]},"p":{"type":"object","x-adc-open-payload":"t"}},
		"allOf":[{"if":{"properties":{"k":{"const":"x"}},"required":["k"]},"then":{"properties":{"p":{"$ref":"#/$defs/ScopeX"}}}}]}`

// roundtrip is compiled against the generated package: it uses every
// generated name it relies on (so a renamed or missing declaration fails the
// build) and checks that documents survive decode and re-encode byte for byte.
const roundtrip = `package main

import (
	"encoding/json"
	"fmt"
	"os"

	"example.com/gen/types"
)

func same[T any](doc string) {
	var v T
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		fail("decode %s: %v", doc, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		fail("encode %s: %v", doc, err)
	}
	if string(out) != doc {
		fail("round trip changed the document:\n  in:  %s\n  out: %s", doc, out)
	}
}

func refused[T any](doc string) {
	var v T
	if err := json.Unmarshal([]byte(doc), &v); err == nil {
		fail("%s decoded, want an error", doc)
	}
}

func fail(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...); os.Exit(1) }

func main() {
	_ = []types.Kind{types.KindPlanApprove, types.KindCouldNotRun}
	_ = types.Map{"a": types.KindPlanApprove}

	// Required arrays and maps keep [] and {}; optional scalars keep false
	// and absence apart; null stays null; properties keep schema order.
	same[types.Opt](` + "`" + `{"z":"","n":0,"l":[],"m":{},"nul":null}` + "`" + `)
	same[types.Opt](` + "`" + `{"z":"s","n":1,"b":false,"l":["x"],"m":{"k":"could_not_run"},"nul":"v","raw":{}}` + "`" + `)
	// Optional collections: absent, empty and populated stay distinct.
	same[types.Opt](` + "`" + `{"z":"","n":0,"l":[],"m":{},"nul":null,"ol":[],"om":{}}` + "`" + `)
	same[types.Opt](` + "`" + `{"z":"","n":0,"l":[],"m":{},"nul":null,"ol":["a"],"om":{"k":"plan.approve"}}` + "`" + `)

	same[types.Set](` + "`" + `"*"` + "`" + `)
	same[types.Set](` + "`" + `["a"]` + "`" + `)
	refused[types.Set]("null")
	refused[types.Set](` + "`" + `"x"` + "`" + `)

	same[types.Evt](` + "`" + `{"a":"e","t":"one"}` + "`" + `)
	same[types.Evt](` + "`" + `{"a":"e","t":"two","n":3}` + "`" + `)
	refused[types.Evt](` + "`" + `{"a":"e","t":"three"}` + "`" + `)
	refused[types.Evt](` + "`" + `{"a":"e","t":"one","n":3}` + "`" + `)
	var e types.Evt
	if err := json.Unmarshal([]byte(` + "`" + `{"a":"e","t":"two","n":3}` + "`" + `), &e); err != nil {
		fail("%v", err)
	}
	if two, ok := e.Variant.(*types.EvtTwo); !ok || two.A != "e" || two.N != 3 {
		fail("variant %#v", e.Variant)
	}

	same[types.Key](` + "`" + `{"v":0}` + "`" + `)
	same[types.Key](` + "`" + `{"d":"x"}` + "`" + `)

	var ref types.Ref = "ab"
	same[types.Cond](` + "`" + `{"o":"y","d":"z","v":"go","r":"` + "`" + ` + string(ref) + ` + "`" + `"}` + "`" + `)
	same[types.Cond](` + "`" + `{"o":"x","d":"","v":null}` + "`" + `)

	var d types.Disp
	if err := json.Unmarshal([]byte(` + "`" + `{"k":"x","p":{"r":2}}` + "`" + `), &d); err != nil {
		fail("%v", err)
	}
	if s, err := d.PAsScopeX(); err != nil || s.R != 2 {
		fail("PAsScopeX = %v, %v", s, err)
	}
	d.K = types.DispKY
	if _, err := d.PAsScopeX(); err == nil {
		fail("accessor ignored the dispatch value")
	}
}
`

// TestGeneratedCodeCompilesAndRoundTrips is the behavioural check of the
// supported grammar: the output is built and run, not inspected as text.
func TestGeneratedCodeCompilesAndRoundTrips(t *testing.T) {
	out, err := Generate(one(supported), "types")
	if err != nil {
		t.Fatal(err)
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go command is needed to compile the generated code: %v", err)
	}
	mod := t.TempDir()
	files := map[string][]byte{
		"go.mod":  []byte("module example.com/gen\n\ngo 1.22\n"),
		"main.go": []byte(roundtrip),
	}
	for name, src := range out {
		files[filepath.Join("types", name)] = src
	}
	for name, src := range files {
		p := filepath.Join(mod, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, src, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(gobin, "run", ".")
	cmd.Dir = mod
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local", "GOWORK=off", "GOPROXY=off", "CGO_ENABLED=0")
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated code does not build or round-trip: %v\n%s", err, msg)
	}
}

func TestDeterministic(t *testing.T) {
	files := one(supported)
	a, err := Generate(files, "types")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		b, err := Generate(files, "types")
		if err != nil {
			t.Fatal(err)
		}
		for name := range a {
			if !bytes.Equal(a[name], b[name]) {
				t.Fatalf("%s differs between runs", name)
			}
		}
	}
}
