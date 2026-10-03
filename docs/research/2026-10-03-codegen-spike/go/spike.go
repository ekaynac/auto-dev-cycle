// SPDX-License-Identifier: Apache-2.0

//go:build ignore

// Codegen spike, Go side: validates ../cases.json against ../schemas with
// santhosh-tekuri/jsonschema/v6. Run from this directory in a scratch module:
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

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	c := jsonschema.NewCompiler()
	for _, f := range []string{"common/defs", "records/receipt", "records/approval", "adapter/event"} {
		b, err := os.ReadFile("../schemas/" + f + ".json")
		must(err)
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(b)))
		must(err)
		must(c.AddResource(base+f+".json", doc))
	}
	schemas := map[string]*jsonschema.Schema{}
	for _, f := range []string{"records/receipt", "records/approval", "adapter/event"} {
		s, err := c.Compile(base + f + ".json")
		must(err)
		schemas[path.Base(f)] = s
	}
	var cases []struct {
		Name, Schema string
		Doc          json.RawMessage
		Want         bool
	}
	b, err := os.ReadFile("../cases.json")
	must(err)
	must(json.Unmarshal(b, &cases))
	bad := 0
	for _, k := range cases {
		inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(k.Doc)))
		must(err)
		got := schemas[k.Schema].Validate(inst) == nil
		mark := "ok  "
		if got != k.Want {
			mark, bad = "BAD ", bad+1
		}
		fmt.Println(mark, k.Name, got)
	}
	if bad > 0 {
		os.Exit(1)
	}
}
