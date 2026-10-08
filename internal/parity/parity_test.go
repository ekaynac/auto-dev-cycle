// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/apidoc"
	"github.com/ekaynac/auto-dev-cycle/internal/cli"
	"github.com/ekaynac/auto-dev-cycle/internal/daemon"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
	"github.com/ekaynac/auto-dev-cycle/schemas"
)

func registry(t *testing.T) *vocab.Registry {
	t.Helper()
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// openapi is the API description's operations, "METHOD /path" → the
// action it carries (x-adc-action; empty for a read or the bootstrap).
func openapi(t *testing.T, reg *vocab.Registry) map[string]string {
	t.Helper()
	b, err := apidoc.Generate(reg.Actions, schemas.FS())
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Paths map[string]map[string]struct {
			Action string `yaml:"x-adc-action"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for p, ops := range d.Paths {
		for method, op := range ops {
			out[strings.ToUpper(method)+" "+p] = op.Action
		}
	}
	return out
}

// answers reports whether the CLI has the command: `--help` succeeds,
// without a controller, where a command exists, and is a usage error
// where it does not.
func answers(t *testing.T, command string) bool {
	t.Helper()
	testguard.Home(t)
	args := append(strings.Fields(command)[1:], "--help")
	return cli.Main(context.Background(), args, io.Discard, io.Discard, nil) == entry.ExitOK
}

// Every registered action is exactly one of wired, deferred and
// unregistered; the explicit deferred list includes review.request (14 D1).
func TestEveryActionClassified(t *testing.T) {
	reg := registry(t)
	actions, err := Actions(reg)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]daemon.Class{}
	for _, a := range actions {
		got[a.Action] = a.Class
		if (a.Class == daemon.Wired) != (a.Via != "") {
			t.Errorf("%s: class %s via %q", a.Action, a.Class, a.Via)
		}
	}
	if !slices.Equal(slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(reg.Actions))) {
		t.Fatalf("matrix %v, registry %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(reg.Actions)))
	}
	if got["review.request"] != daemon.Deferred {
		t.Errorf("review.request is %s, not deferred", got["review.request"])
	}
}

// Every action has its endpoint in the API description and a CLI command
// that the CLI answers; every CLI action command is a registered action.
func TestActionParity(t *testing.T) {
	reg := registry(t)
	actions, err := Actions(reg)
	if err != nil {
		t.Fatal(err)
	}
	ops := openapi(t, reg)
	for _, a := range actions {
		if action, ok := ops[a.Endpoint]; !ok || action != a.Action {
			t.Errorf("%s: no %s in the API description (%q)", a.Action, a.Endpoint, action)
		}
		if a.CLI == "" || !answers(t, a.CLI) {
			t.Errorf("%s (%s): no CLI command %q", a.Action, a.Class, a.CLI)
		}
	}
	for action, command := range cli.Commands() {
		if _, ok := reg.Actions[action]; !ok {
			t.Errorf("%s names %s, which is not a registered action", command, action)
		}
	}
	if ops[Bootstrap.Endpoint] != "" || !answers(t, Bootstrap.CLI) {
		t.Errorf("bootstrap: %s or %s is missing", Bootstrap.Endpoint, Bootstrap.CLI)
	}
}

// Every read has its endpoint and a CLI command the CLI answers, and every
// CLI read command is a served read. Every operation of the API
// description is an action's, a read's or the bootstrap's.
func TestReadParity(t *testing.T) {
	reg := registry(t)
	ops := openapi(t, reg)
	known := map[string]bool{Bootstrap.Endpoint: true, "GET " + api.ReadPath: true}
	for action := range reg.Actions {
		known["POST "+apidoc.CommandPath(action)] = true
	}
	for _, r := range Reads() {
		known[r.Endpoint] = true
		if _, ok := ops[r.Endpoint]; !ok {
			t.Errorf("%s: no %s in the API description", r.Resource, r.Endpoint)
		}
		if r.CLI == "" || !answers(t, r.CLI) {
			t.Errorf("%s: no CLI command %q", r.Resource, r.CLI)
		}
	}
	for op := range ops {
		if !known[op] {
			t.Errorf("%s is in the API description and is no action, read or bootstrap", op)
		}
	}
	for name, command := range readCommands {
		if !slices.ContainsFunc(Reads(), func(r Read) bool { return r.Resource == name }) {
			t.Errorf("%s reads %s, which is not served", command, name)
		}
	}
}

// Every route the daemon serves is in the API description (the command
// route through each action's endpoint) or a listed exception, and every
// operation of the description is served by its route.
func TestServedRoutes(t *testing.T) {
	ops := openapi(t, registry(t))
	mux := http.NewServeMux()
	for _, route := range daemon.Routes() {
		mux.Handle(route, http.NotFoundHandler())
		_, documented := ops[route]
		_, excepted := UndocumentedRoutes[route]
		switch {
		case route == "POST "+daemon.CommandsPath: // TestActionParity: one endpoint per action
		case documented && excepted:
			t.Errorf("%s is documented now; remove it from UndocumentedRoutes", route)
		case !documented && !excepted:
			t.Errorf("the daemon serves %s, which the API description does not carry", route)
		}
	}
	for route := range UndocumentedRoutes {
		if !slices.Contains(daemon.Routes(), route) {
			t.Errorf("undocumented route %s is not served", route)
		}
	}
	// Each operation is served by its own route, a command by the command
	// route and a registered resource by the resource route.
	for op := range ops {
		method, path, _ := strings.Cut(op, " ")
		want := op
		if _, registered := api.Registered()[strings.TrimPrefix(path, "/v1/")]; registered {
			want = "GET " + api.ReadPath
		} else if strings.HasPrefix(path, apidoc.CommandPath("")) {
			want = "POST " + daemon.CommandsPath
		}
		if _, got := mux.Handler(httptest.NewRequest(method, strings.ReplaceAll(path, "{resource}", "x"), nil)); got != want {
			t.Errorf("%s is served by %q, not %q", op, got, want)
		}
	}
}

// A read without a CLI command is refused rather than rendered.
func TestMarkdownRefusesUnmappedRead(t *testing.T) {
	actions, err := Actions(registry(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Markdown(actions, Reads()); err != nil {
		t.Fatal(err)
	}
	if _, err := Markdown(actions, []Read{{Resource: "x", Endpoint: "GET /v1/x"}}); err == nil {
		t.Error("a read without a CLI command rendered")
	}
}

// The probe tells a command from none: a sub-command a noun lacks, and a
// noun that is no command, are not answered.
func TestAnswersProbe(t *testing.T) {
	for command, want := range map[string]bool{"adc seat take": true, "adc admin bootstrap": true, "adc seat frobnicate": false, "adc frobnicate": false} {
		if got := answers(t, command); got != want {
			t.Errorf("%s: %v", command, got)
		}
	}
}
