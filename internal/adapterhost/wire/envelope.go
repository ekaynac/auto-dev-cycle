// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

// Version is the JSON-RPC version every message carries.
const Version = "2.0"

// EventMethod is the notification that carries adapter events.
const EventMethod = "adapter.event"

// Request is a core → adapter call. Params is omitted when empty.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("adapter error %d: %s", e.Code, e.Message) }

// Kind is what an accepted message is.
type Kind int

const (
	// KindResponse answers a request: Response is set.
	KindResponse Kind = iota + 1
	// KindEvent is an adapter.event notification: Event holds its params.
	KindEvent
)

// Response is an adapter's reply: exactly one of Result and Error is set
// (Result holds the raw JSON, which may be null).
type Response struct {
	ID     int64
	Result json.RawMessage
	Error  *RPCError
}

// Message is one classified line.
type Message struct {
	Kind     Kind
	Response *Response
	Event    json.RawMessage // the params object of an adapter.event
}

// Refusal is a line that is not an acceptable message. Stage is "json" for
// a line that is not strict JSON (contract.ReadStrict) and "envelope" for
// strict JSON that is neither a response nor an adapter.event notification.
// The session continues past a refused line; what it means is the caller's.
type Refusal struct {
	Stage  string
	Detail string
}

func (r *Refusal) Error() string { return "adapter message refused (" + r.Stage + "): " + r.Detail }

// Classify reads one line (from Reader.Next) strictly and classifies it as
// the conformance kit does: a response has an integer id, no method, and
// exactly one of result and error, where error is an object with an integer
// code and a string message; an event has method adapter.event, no id, and
// object params. Unknown members are allowed.
func Classify(line []byte) (Message, error) {
	doc, err := contract.ReadStrict(line)
	if err != nil {
		return Message{}, &Refusal{Stage: "json", Detail: err.Error()}
	}
	envelope := func(detail string) (Message, error) {
		return Message{}, &Refusal{Stage: "envelope", Detail: detail}
	}
	m, ok := doc.(map[string]any)
	if !ok || m["jsonrpc"] != Version {
		return envelope("not a JSON-RPC 2.0 message")
	}
	raw, err := members(line)
	if err != nil {
		return Message{}, err
	}
	_, hasID := m["id"]
	_, hasMethod := m["method"]
	_, hasResult := m["result"]
	errValue, hasError := m["error"]
	if m["method"] == EventMethod && !hasID {
		if _, ok := m["params"].(map[string]any); !ok {
			return envelope("an adapter.event without object params")
		}
		return Message{Kind: KindEvent, Event: raw["params"]}, nil
	}
	id, isNumber := m["id"].(json.Number)
	if !isNumber || hasMethod || hasResult == hasError {
		return envelope("neither a response nor an adapter.event notification")
	}
	r := &Response{Result: raw["result"]}
	if r.ID, err = id.Int64(); err != nil { // ReadStrict bounds every integer
		return Message{}, fmt.Errorf("wire: id %s: %w", id, err)
	}
	if hasError {
		e, isObject := errValue.(map[string]any)
		_, codeIsNumber := e["code"].(json.Number)
		_, messageIsString := e["message"].(string)
		if !isObject || !codeIsNumber || !messageIsString {
			return envelope("an error that is not a JSON-RPC error object")
		}
		r.Error = &RPCError{Message: e["message"].(string)}
		if r.Error.Code, err = e["code"].(json.Number).Int64(); err != nil {
			return Message{}, fmt.Errorf("wire: error code: %w", err)
		}
		members, err := members(raw["error"])
		if err != nil {
			return Message{}, err
		}
		r.Error.Data = members["data"]
	}
	return Message{Kind: KindResponse, Response: r}, nil
}

// members returns an object's members as their bytes, keys exactly as sent
// (a line ReadStrict accepted has no duplicate keys).
func members(object []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(object, &m); err != nil {
		return nil, fmt.Errorf("wire: re-reading a strict object: %w", err)
	}
	return m, nil
}

// Encode marshals a message for Writer.Write: no HTML escaping (as
// JSON.stringify writes) and no invalid UTF-8.
func Encode(v any) ([]byte, error) {
	b, err := jsonv2.Marshal(v, json.DefaultOptionsV1(), jsontext.AllowInvalidUTF8(false), jsontext.EscapeForHTML(false))
	if err != nil {
		return nil, fmt.Errorf("wire: encode: %w", err)
	}
	return b, nil
}
