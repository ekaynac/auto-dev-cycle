// SPDX-License-Identifier: Apache-2.0

package contract

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// approval is a hand-written stand-in for the generated type (step 2b-2).
type approval struct {
	ID            string          `json:"id"`
	Actor         string          `json:"actor"`
	ActorKind     string          `json:"actor_kind"`
	Action        string          `json:"action"`
	ChangeClass   []string        `json:"change_class"`
	Project       *string         `json:"project"`
	Object        object          `json:"object"`
	ExpectedState expectedState   `json:"expected_state"`
	Scope         json.RawMessage `json:"scope"`
	Grant         *string         `json:"grant"`
	Confirmation  confirmation    `json:"confirmation"`
	Answers       []string        `json:"answers"`
	Supersedes    []string        `json:"supersedes"`
	Question      *string         `json:"question"`
	AnswerVerbat  *string         `json:"answer_verbatim"`
	Reason        string          `json:"reason"`
	At            string          `json:"at"`
}

type object struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Version int64  `json:"version"`
	Digest  string `json:"digest"`
}

type expectedState struct {
	Version *int64  `json:"version,omitempty"`
	Digest  *string `json:"digest,omitempty"`
}

type confirmation struct {
	Policy      string   `json:"policy"`
	Challenge   *string  `json:"challenge"`
	SatisfiedBy []string `json:"satisfied_by"`
}

const validApproval = `{"id":"apr_01h455vb4pex5vsknk084sn006","actor":"act_01h455vb4pex5vsknk084sn007",
"actor_kind":"operator","action":"plan.approve","change_class":[],"project":null,
"object":{"type":"plan","id":"p","version":3,"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
"expected_state":{"version":3},"scope":{"plan_revision":3},"grant":null,
"confirmation":{"policy":"none","challenge":null,"satisfied_by":[]},"answers":[],"supersedes":[],
"question":null,"answer_verbatim":null,"reason":"r","at":"2026-10-03T10:00:00Z"}`

func contractError(t *testing.T, err error, want string) {
	t.Helper()
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("want a contract error containing %q, got %v", want, err)
	}
	if !strings.Contains(ce.Detail, want) {
		t.Fatalf("want detail containing %q, got %q", want, ce.Detail)
	}
}

func TestEveryEmbeddedSchemaCompiles(t *testing.T) {
	all, err := load()
	if err != nil {
		t.Fatalf("embedded schemas: %v", err)
	}
	for _, s := range []string{"common/defs", "records/receipt", "records/approval", "records/grant", "adapter/event"} {
		if all[s] == nil {
			t.Errorf("schema %s not compiled", s)
		}
	}
}

func TestDecodeValid(t *testing.T) {
	a, err := Decode[approval]("records/approval", []byte(validApproval))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if a.ExpectedState.Version == nil || *a.ExpectedState.Version != 3 || a.ExpectedState.Digest != nil {
		t.Errorf("expected_state decoded as %+v", a.ExpectedState)
	}
	if string(a.Scope) != `{"plan_revision":3}` {
		t.Errorf("scope = %s", a.Scope)
	}
}

func TestDecodeRefuses(t *testing.T) {
	cases := []struct{ name, doc, want string }{
		{"schema violation", strings.Replace(validApproval, `"operator"`, `"root"`, 1), "actor_kind"},
		{"trailing data", validApproval + ` {}`, "trailing data"},
		{"non-integral spelling", strings.Replace(validApproval, `"version":3,`, `"version":3.0,`, 1), "3.0"},
		{"exponent spelling", strings.Replace(validApproval, `"version":3,`, `"version":3e0,`, 1), "3e0"},
		{"not JSON", `{"id":`, "not JSON"},
		{"negative zero", strings.Replace(validApproval, `"version":3,`, `"version":-0,`, 1), "-0"},
		// encoding/json keeps the last duplicate; JCS refuses it. Refuse at the boundary.
		{"duplicate key", strings.Replace(validApproval, `"action":"plan.approve",`, `"action":"plan.approve","action":"input.answer",`, 1), `duplicate object key "action"`},
		{"nested duplicate key", strings.Replace(validApproval, `"scope":{"plan_revision":3}`, `"scope":{"plan_revision":3,"plan_revision":4}`, 1), `duplicate object key "plan_revision"`},
		{"duplicate key in an array element", `[{"a":1,"a":1}]`, `duplicate object key "a"`},
		{"invalid UTF-8", strings.Replace(validApproval, `"reason":"r"`, "\"reason\":\"r\xff\"", 1), "invalid UTF-8"},
		{"truncated", `{"id":"x"`, "not JSON"},
		{"too deep", strings.Repeat("[", 10002) + strings.Repeat("]", 10002), "max depth"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Decode[approval]("records/approval", []byte(c.doc))
			contractError(t, err, c.want)
		})
	}
}

// A valid document that the Go type cannot hold exactly is an error, never a
// silent drop: the type misses answer_verbatim.
func TestDecodeRefusesFieldsTheTypeLacks(t *testing.T) {
	type partial struct {
		ID string `json:"id"`
	}
	_, err := Decode[partial]("records/approval", []byte(validApproval))
	contractError(t, err, "does not fit the Go type")
}

func TestUnknownSchema(t *testing.T) {
	contractError(t, Validate("records/nope", []byte(`{}`)), "unknown schema")
}

func TestEncodeValidatesOutput(t *testing.T) {
	a, err := Decode[approval]("records/approval", []byte(validApproval))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if _, err := Encode("records/approval", a); err != nil {
		t.Fatalf("Encode of a decoded valid document: %v", err)
	}
	a.Reason = "<a & b>"
	out, err := Encode("records/approval", a)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(out), `"reason":"<a & b>"`) || strings.HasSuffix(string(out), "\n") {
		t.Errorf("Encode escaped HTML or kept the encoder newline: %s", out)
	}
	d := "sha256:" + strings.Repeat("b", 64)
	a.ExpectedState.Digest = &d // now both keys: the keyed union forbids it
	_, err = Encode("records/approval", a)
	contractError(t, err, "expected_state")
}
