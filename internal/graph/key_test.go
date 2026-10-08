// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	_ "github.com/ekaynac/auto-dev-cycle/internal/vocab" // the reason check
)

const (
	nodID  = "nod_01m3250v57fhm9v1z9xd1wk8gw"
	planID = "nod_01m3250v57ft7aehkqwq0yrq3n"
	ownID  = "own_01m3250vz2ezn81cgvbkcn17jd"
	resA   = "res_01m3250v57fhm9v1z9xd1wk8gw"
	resB   = "res_01m3250v57ft7aehkqwq0yrq3n"
	aprA   = "apr_01m3250vz2ezn81cgvbkcn17jd"
	aprB   = "apr_01m3250vnfeyrv67x6w0ddqr6a"
)

// node is a review with a gate result input and an answer input.
func node() types.Node {
	return types.Node{ID: nodID, Kind: types.NodeKindReview, DefinitionVersion: "1",
		Inputs: []types.InputSpec{
			{Name: "gate", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: planID, ResultKind: types.ResultKindGateResult}}},
			{Name: "ruling", Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: ownID, Revision: 2}}},
		},
		Params: json.RawMessage(`{"roster":[{"executor":"exe_x","family":"claude"}],"max_turns":8}`), Round: 1}
}

func resolved() map[string]Resolved {
	return map[string]Resolved{"gate": {Result: resA}, "ruling": {Approval: aprA, Revision: 2}}
}

func key(t *testing.T, n types.Node, r map[string]Resolved, gen int64) types.ComputationKey {
	t.Helper()
	k, err := Key(n, r, gen)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The framing, computed outside Go:
//
//	python3 -c 'import hashlib,json; p="sha256:"+hashlib.sha256(json.dumps({"max_turns":8,"roster":[{"executor":"exe_x","family":"claude"}]},
//	  sort_keys=True,separators=(",",":")).encode()).hexdigest(); d={"kind":"review","definition_version":"1","params":p,
//	  "inputs":[{"name":"gate","result":"res_01m3250v57fhm9v1z9xd1wk8gw"},{"name":"ruling","approval":"apr_01m3250vz2ezn81cgvbkcn17jd","revision":2}],
//	  "generation":1}; print("sha256:"+hashlib.sha256(json.dumps(d,sort_keys=True,separators=(",",":")).encode()).hexdigest())'
const wantFraming = "sha256:e37def4ea92c4cd3fd6cf1c780e1d0e4cf202ef208c3a7d0eb4f45cbc78679ab"

func TestKeyFraming(t *testing.T) {
	if got, want := key(t, node(), resolved(), 1).Key, types.Digest(wantFraming); got != want {
		t.Fatalf("key %s, want %s", got, want)
	}
}

func TestKeyIsDeterministic(t *testing.T) {
	a := key(t, node(), resolved(), 1)
	if b := key(t, node(), resolved(), 1); a != b {
		t.Fatalf("%v then %v", a, b)
	}
	// The params' spelling and the declared order are not part of the key.
	n := node()
	n.Params = json.RawMessage("{ \"max_turns\": 8,\n \"roster\": [ {\"family\": \"claude\", \"executor\": \"exe_x\"} ] }")
	n.Inputs[0], n.Inputs[1] = n.Inputs[1], n.Inputs[0]
	if b := key(t, n, resolved(), 1); a != b {
		t.Fatalf("a respelling or reordering changed the key: %v, %v", a, b)
	}
}

func TestEveryPartChangesTheKey(t *testing.T) {
	base := key(t, node(), resolved(), 1).Key
	for name, c := range map[string]struct {
		edit func(n *types.Node, r map[string]Resolved)
		gen  int64
	}{
		"another gate result": {func(_ *types.Node, r map[string]Resolved) { r["gate"] = Resolved{Result: resB} }, 1},
		"another approval":    {func(_ *types.Node, r map[string]Resolved) { r["ruling"] = Resolved{Approval: aprB, Revision: 2} }, 1},
		"another answer revision": {func(n *types.Node, r map[string]Resolved) {
			n.Inputs[1].Source.Variant.(*types.AnswerSource).Revision, r["ruling"] = 3, Resolved{Approval: aprA, Revision: 3}
		}, 1},
		"another params value": {func(n *types.Node, _ map[string]Resolved) {
			n.Params = json.RawMessage(`{"roster":[{"executor":"exe_x","family":"claude"}],"max_turns":9}`)
		}, 1},
		"the next generation": {func(*types.Node, map[string]Resolved) {}, 2},
		"another kind":        {func(n *types.Node, _ map[string]Resolved) { n.Kind = types.NodeKindGate }, 1},
		"another definition":  {func(n *types.Node, _ map[string]Resolved) { n.DefinitionVersion = "2" }, 1},
		"inputs swapped by name": {func(n *types.Node, r map[string]Resolved) {
			n.Inputs[0].Name, n.Inputs[1].Name = "ruling", "gate"
			r["gate"], r["ruling"] = r["ruling"], r["gate"]
		}, 1},
	} {
		n, r := node(), resolved()
		c.edit(&n, r)
		if k := key(t, n, r, c.gen); k.Key == base {
			t.Errorf("%s: the key did not change", name)
		}
	}
}

// A generation bump re-keys the same node: the node is not touched.
func TestGenerationBumpReKeysTheSameNode(t *testing.T) {
	n := node()
	before, _ := json.Marshal(n)
	k1, k2 := key(t, n, resolved(), 1), key(t, n, resolved(), 2)
	after, _ := json.Marshal(n)
	if k1.Key == k2.Key || k2.Generation != 2 || string(before) != string(after) {
		t.Fatalf("%v, %v; node changed %t", k1, k2, string(before) != string(after))
	}
}

func TestKeyRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(n *types.Node, r map[string]Resolved)
		gen  int64
	}{
		"params integer past 2^53-1": {func(n *types.Node, _ map[string]Resolved) {
			n.Params = json.RawMessage(`{"max_turns":9007199254740992}`)
		}, 1},
		"params integer below -(2^53-1)": {func(n *types.Node, _ map[string]Resolved) {
			n.Params = json.RawMessage(`{"max_turns":-9007199254740992}`)
		}, 1},
		"params fraction":                     {func(n *types.Node, _ map[string]Resolved) { n.Params = json.RawMessage(`{"max_turns":1.5}`) }, 1},
		"params exponent":                     {func(n *types.Node, _ map[string]Resolved) { n.Params = json.RawMessage(`{"max_turns":1e3}`) }, 1},
		"params negative zero":                {func(n *types.Node, _ map[string]Resolved) { n.Params = json.RawMessage(`{"max_turns":-0}`) }, 1},
		"params duplicate key":                {func(n *types.Node, _ map[string]Resolved) { n.Params = json.RawMessage(`{"a":1,"a":2}`) }, 1},
		"params trailing data":                {func(n *types.Node, _ map[string]Resolved) { n.Params = json.RawMessage(`{} {}`) }, 1},
		"generation 0":                        {func(*types.Node, map[string]Resolved) {}, 0},
		"generation past 2^53-1":              {func(*types.Node, map[string]Resolved) {}, 1 << 53},
		"no definition version":               {func(n *types.Node, _ map[string]Resolved) { n.DefinitionVersion = "" }, 1},
		"an unresolved input":                 {func(_ *types.Node, r map[string]Resolved) { delete(r, "gate") }, 1},
		"an undeclared resolution":            {func(_ *types.Node, r map[string]Resolved) { r["extra"] = Resolved{Result: resB} }, 1},
		"a result input as an approval":       {func(_ *types.Node, r map[string]Resolved) { r["gate"] = Resolved{Approval: aprA, Revision: 2} }, 1},
		"an answer input as a result":         {func(_ *types.Node, r map[string]Resolved) { r["ruling"] = Resolved{Result: resB} }, 1},
		"a result input with an approval too": {func(_ *types.Node, r map[string]Resolved) { r["gate"] = Resolved{Result: resA, Approval: aprA} }, 1},
		"a result input with a revision too":  {func(_ *types.Node, r map[string]Resolved) { r["gate"] = Resolved{Result: resA, Revision: 2} }, 1},
		"an answer input with a result too": {func(_ *types.Node, r map[string]Resolved) {
			r["ruling"] = Resolved{Result: resB, Approval: aprA, Revision: 2}
		}, 1},
		"an answer of another revision":  {func(_ *types.Node, r map[string]Resolved) { r["ruling"] = Resolved{Approval: aprA, Revision: 1} }, 1},
		"a result id of another kind":    {func(_ *types.Node, r map[string]Resolved) { r["gate"] = Resolved{Result: aprA} }, 1},
		"an approval id of another kind": {func(_ *types.Node, r map[string]Resolved) { r["ruling"] = Resolved{Approval: resA, Revision: 2} }, 1},
		"a duplicate input name": {func(n *types.Node, _ map[string]Resolved) {
			n.Inputs = append(n.Inputs, n.Inputs[0])
		}, 1},
	} {
		n, r := node(), resolved()
		c.edit(&n, r)
		if _, err := Key(n, r, c.gen); !errors.Is(err, ErrKey) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
