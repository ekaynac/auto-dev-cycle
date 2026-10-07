// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/cli"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

func matrix(t *testing.T, reg *vocab.Registry) map[string]Row {
	t.Helper()
	rows, err := Matrix(reg)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Row{}
	for _, r := range rows {
		out[r.Action] = r
	}
	return out
}

// Each class answers as the matrix says, through the running daemon: a
// deferred action, from its CLI command, is could_not_run/action_deferred
// (exit 2) with no command committed; a wired one is never answered
// action_deferred.
func TestMatrixAnswers(t *testing.T) {
	c := runCommanded(t)
	op, cmds, rows := c.op.Reveal(), cli.Commands(), matrix(t, mustLoad(t))
	before := c.commandsOf(t)
	for _, action := range slices.Sorted(maps.Keys(rows)) {
		if rows[action].Class != Deferred {
			continue
		}
		cmd, ok := cmds[action]
		if !ok {
			t.Errorf("deferred %s has no CLI command", action)
			continue
		}
		args := append(strings.Fields(cmd)[1:], "--expect", "0", "--reason", "r", "--json")
		if code, out, errb := c.adc(op, args...); code != entry.ExitCouldNotRun {
			t.Errorf("%s: exit %d %s %s", action, code, out, errb)
		} else {
			refusedAs(t, response(t, out), vocab.CouldNotRun, vocab.ReasonActionDeferred)
		}
	}
	if n := c.commandsOf(t); n != before {
		t.Errorf("deferred actions committed %d commands", n-before)
	}
	for _, action := range slices.Sorted(maps.Keys(rows)) {
		switch rows[action].Class {
		case Deferred:
		case Wired:
			// An empty scope reaches the handler: refused as invalid (400)
			// by its derivation, or decided by it; never deferred.
			code, _, raw := c.post(t, action, op, strings.NewReader(strings.Replace(good, `"k"`, `"matrix-`+action+`"`, 1)))
			switch {
			case code == http.StatusBadRequest && strings.Contains(string(raw), "scope is not valid"):
			case code != http.StatusOK:
				t.Errorf("wired %s: HTTP %d %s", action, code, raw)
			default:
				if v, ok := response(t, string(raw)).(*types.CommandRefused); ok && v.Reason == vocab.ReasonActionDeferred {
					t.Errorf("wired %s is answered action_deferred", action)
				}
			}
		case Unregistered: // none in the current registry; TestMatrixDeprecated covers each
			_, _, raw := c.post(t, action, op, strings.NewReader(good))
			refusedAs(t, response(t, string(raw)), vocab.Failed, vocab.ReasonActionUnregistered)
		default:
			t.Errorf("%s: class %q", action, rows[action].Class)
		}
	}
}

// Every action, once deprecated, is unregistered in the matrix and is
// refused failed/action_unregistered before anything else.
func TestMatrixDeprecated(t *testing.T) {
	for _, action := range slices.Sorted(maps.Keys(matrix(t, mustLoad(t)))) {
		reg := mustLoad(t)
		a := reg.Actions[action]
		since := "0.1.0"
		a.Deprecated = &since
		reg.Actions[action] = a
		if r := matrix(t, reg)[action]; r.Class != Unregistered {
			t.Errorf("deprecated %s: %s", action, r.Class)
		}
		// Every action's request layer only records that it was reached:
		// a deprecated action never reaches one, so nothing is submitted.
		reached := false
		ls, is := map[string]logic{}, map[string]issuer{}
		for _, a := range slices.Collect(maps.Keys(reg.Actions)) {
			ls[a] = func(eventstore.Command) eventstore.Decide { reached = true; return nil }
		}
		v, err := commands{reg: reg, logic: ls, issue: is}.decide(context.Background(), identity.Principal{}, action, types.CommandRequest{})
		if err != nil || reached {
			t.Fatalf("%s: %v, request layer reached %v", action, err, reached)
		}
		refusedAs(t, v, vocab.Failed, vocab.ReasonActionUnregistered)
	}
}

// Matrix refuses an unclassified action, one both wired and deferred, and
// a deferred action the registry does not register.
func TestMatrixRefusesMisclassification(t *testing.T) {
	savedDeferred, savedNotes := deferred, wiredNotes
	t.Cleanup(func() { deferred, wiredNotes = savedDeferred, savedNotes })
	with := func(edit func(map[string]Owner)) map[string]Owner {
		m := maps.Clone(savedDeferred)
		edit(m)
		return m
	}
	for name, list := range map[string]map[string]Owner{
		"unclassified":   with(func(m map[string]Owner) { delete(m, "review.request") }),
		"wired+deferred": with(func(m map[string]Owner) { m["seat.take"] = Owner{Milestone: Unassigned} }),
		"unregistered":   with(func(m map[string]Owner) { m["no.such"] = Owner{Milestone: Unassigned} }),
		"no milestone":   with(func(m map[string]Owner) { m["task.create"] = Owner{} }),
	} {
		deferred = list
		if _, err := Matrix(mustLoad(t)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	deferred, wiredNotes = savedDeferred, map[string]string{"task.create": "a note on a deferred action"}
	if _, err := Matrix(mustLoad(t)); err == nil {
		t.Error("a note on an unwired action: no error")
	}
}

// The served mux is the one handler registered before the route list:
// each pattern routes as before, the bootstrap only with a bootstrapper,
// and nothing else is served.
func TestRoutesUnchanged(t *testing.T) {
	want := []string{"POST /v1/bootstrap", "GET /v1/events", "GET /v1/version", "POST /v1/commands/{action}", "GET /v1/whoami", "GET /v1/{resource}"}
	if !slices.Equal(Routes(), want) {
		t.Fatalf("routes %v", Routes())
	}
	pattern := func(boot *bootstrapper, method, path string) string {
		_, p := routes(nil, boot, limits{}, nil, commands{}).Handler(httptest.NewRequest(method, path, nil))
		return p
	}
	for route, path := range map[string]string{want[0]: "/v1/bootstrap", want[1]: "/v1/events", want[2]: "/v1/version",
		want[3]: "/v1/commands/seat.take", want[4]: "/v1/whoami", want[5]: "/v1/grants"} {
		method, _, _ := strings.Cut(route, " ")
		if got := pattern(&bootstrapper{}, method, path); got != route {
			t.Errorf("%s %s routes to %q", method, path, got)
		}
	}
	for _, req := range [][3]string{{"", "POST", "/v1/bootstrap"}, {"boot", "GET", "/v1/bootstrap/x"}, {"boot", "DELETE", "/v1/events"},
		{"boot", "GET", "/v1/commands/seat.take"}, {"boot", "POST", "/v1/grants"}, {"boot", "GET", "/v2/events"}} {
		var boot *bootstrapper
		if req[0] != "" {
			boot = &bootstrapper{}
		}
		if got := pattern(boot, req[1], req[2]); got != "" {
			t.Errorf("%v routes to %q", req, got)
		}
	}
}

func mustLoad(t *testing.T) *vocab.Registry {
	t.Helper()
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}
