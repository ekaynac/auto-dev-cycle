// SPDX-License-Identifier: Apache-2.0

//go:build ignore

// Codegen spike, Go side: validates ../cases.json against ../schemas with
// santhosh-tekuri/jsonschema/v6, then checks the cases' adequacy: each schema
// mutation below (one keyword removed everywhere) must flip at least one case.
// Run from this directory in a scratch module:
// `go mod init spike && go get github.com/santhosh-tekuri/jsonschema/v6@v6.0.3 && go run spike.go`.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const base = "https://github.com/ekaynac/auto-dev-cycle/schemas/"

var files = []string{"common/defs", "records/receipt", "records/approval", "records/grant", "adapter/event"}

type testCase struct {
	Name, Schema string
	Doc          json.RawMessage
	Want         bool
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// strip removes keyword from every object in the schema tree.
func strip(v any, keyword string) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range t {
			if k != keyword {
				out[k] = strip(x, keyword)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = strip(x, keyword)
		}
		return out
	}
	return v
}

// run validates every case with the schemas after removing keyword ("" = none)
// and returns the number of cases classified differently from Want.
func run(cases []testCase, keyword string, verbose bool) int {
	c := jsonschema.NewCompiler()
	for _, f := range files {
		b, err := os.ReadFile("../schemas/" + f + ".json")
		must(err)
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(b)))
		must(err)
		if keyword != "" {
			doc = strip(doc, keyword)
		}
		must(c.AddResource(base+f+".json", doc))
	}
	schemas := map[string]*jsonschema.Schema{}
	for _, f := range files[1:] {
		s, err := c.Compile(base + f + ".json")
		must(err)
		schemas[path.Base(f)] = s
	}
	bad := 0
	for _, k := range cases {
		inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(k.Doc)))
		must(err)
		got := schemas[k.Schema].Validate(inst) == nil
		if got != k.Want {
			bad++
		}
		if verbose {
			mark := "ok  "
			if got != k.Want {
				mark = "BAD "
			}
			fmt.Println(mark, k.Name, got)
		}
	}
	return bad
}

func main() {
	var cases []testCase
	b, err := os.ReadFile("../cases.json")
	must(err)
	must(json.Unmarshal(b, &cases))
	if bad := run(cases, "", true); bad > 0 {
		fmt.Printf("%d of %d cases misclassified\n", bad, len(cases))
		os.Exit(1)
	}
	fmt.Printf("%d of %d cases classified correctly\n", len(cases), len(cases))

	weak := 0
	for _, kw := range []string{"additionalProperties", "unevaluatedProperties", "required", "const", "enum",
		"pattern", "minimum", "maximum", "uniqueItems", "minItems", "propertyNames", "allOf", "oneOf"} {
		flipped := run(cases, kw, false)
		mark := "ok  "
		if flipped == 0 {
			mark, weak = "WEAK", weak+1
		}
		fmt.Printf("%s mutation: without %-22s %d case(s) flip\n", mark, kw, flipped)
	}
	if weak > 0 {
		os.Exit(1)
	}
}
