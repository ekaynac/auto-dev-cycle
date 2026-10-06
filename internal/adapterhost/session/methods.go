// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

// The typed methods of 09 over the transport, each with its checks, and the
// session's attempt fence: a session serves exactly one attempt. One prepare
// the adapter accepts (empty or found), with no other prepare or run
// overlapping it, fixes the attempt and fencing token; run, tool.response
// and cancel must name them (tool.response also the pending run's call),
// and every handle either way must be that attempt's.

// ContractMajor is the contract major this core speaks (09 "Versioning").
const ContractMajor = 1

const (
	// KindVersion: describe named another contract major.
	KindVersion Kind = "version"
	// KindForeignHandle: an event or the run's result holds a handle naming
	// another attempt than the session's (09: refused, unsafe_output).
	KindForeignHandle Kind = "foreign_handle"
)

// CheckForeignHandle is the refusal of a handle of another attempt, as
// attempt.output_refused names it (09 "Blob exchange").
const CheckForeignHandle = types.AttemptOutputRefusalCheckForeignHandle

// ErrForeignParams refuses, before anything is sent, params of the core's
// that name another attempt, fencing token or call than the session's
// prepared attempt and pending run, or hold a handle of another attempt.
var ErrForeignParams = errors.New("adapterhost: the params name another attempt, fencing token or call than the session's")

// ErrSequence refuses, before anything is sent, a prepare that is not the
// session's first, overlaps another prepare, or comes after one was
// accepted; and a run before a prepare was accepted.
var ErrSequence = errors.New("adapterhost: a session prepares once, with nothing overlapping, before it runs")

// fence is the session's attempt state, under the session's lock.
type fence struct {
	preparing bool
	prepared  *stamp // the accepted prepare's attempt and fencing token
}

// result is a check: the result decoded against its schema, an invalid one
// a protocol error.
func result[T any](method, schema string) func(json.RawMessage) (any, *Error) {
	return func(raw json.RawMessage) (any, *Error) {
		v, err := contract.Decode[T](schema, raw)
		if err != nil {
			return nil, &Error{Kind: KindProtocol, Method: method, Detail: fmt.Sprintf("the %s result does not match %s", method, schema)}
		}
		return v, nil
	}
}

// typed is a call's value as its method's type.
func typed[T any](v any, err error) (T, error) {
	t, _ := v.(T)
	return t, err
}

// Describe asks for the adapter's description and refuses another contract
// major, which ends the session (checked in the reader, also for a reply
// nobody waits for any more).
func (s *Session) Describe(ctx context.Context) (types.AdapterDescribeResult, error) {
	decode := result[types.AdapterDescribeResult]("describe", "adapter/describe-result")
	return typed[types.AdapterDescribeResult](s.call(ctx, "describe", "", nil, nil, expect{check: func(raw json.RawMessage) (any, *Error) {
		v, e := decode(raw)
		if e != nil {
			return nil, e
		}
		if major, _, _ := strings.Cut(v.(types.AdapterDescribeResult).ContractVersion, "."); major != strconv.Itoa(ContractMajor) {
			return nil, &Error{Kind: KindVersion, Method: "describe", Detail: fmt.Sprintf("another contract major than %d", ContractMajor)}
		}
		return v, nil
	}}))
}

// Prepare prepares the session's one attempt; its inputs must be handles of
// that attempt. It is refused (ErrSequence) once a prepare was accepted or
// while one is pending. A prepare the adapter refuses, by an error or an
// outcome other than empty or found, leaves the session unprepared.
func (s *Session) Prepare(ctx context.Context, p types.AdapterPrepareParams) (types.AdapterOutcome, error) {
	if b, err := json.Marshal(p); err != nil || foreign(b, p.Attempt) {
		return types.AdapterOutcome{}, ErrForeignParams
	}
	decode := result[types.AdapterOutcome]("prepare", "adapter/outcome")
	settle := func(prepared *stamp) { // in the reader, before delivery
		s.mu.Lock()
		s.fence = fence{prepared: prepared}
		s.mu.Unlock()
	}
	return typed[types.AdapterOutcome](s.call(ctx, "prepare", "adapter/prepare-params", p, nil, expect{
		admit: func() error {
			if s.fence.preparing || s.fence.prepared != nil || s.run != nil {
				return ErrSequence
			}
			s.fence.preparing = true
			return nil
		},
		check: func(raw json.RawMessage) (any, *Error) {
			v, e := decode(raw)
			if e != nil {
				return nil, e
			}
			var prepared *stamp
			if o := v.(types.AdapterOutcome).Outcome; o == types.OutcomeEmpty || o == types.OutcomeFound {
				prepared = &stamp{Attempt: p.Attempt, FencingToken: p.FencingToken}
			}
			settle(prepared)
			return v, nil
		},
		refused: func() { settle(nil) },
	}))
}

// Run runs the call. onEvent sees each validated event, stamped for this
// run, on the session's reader: it must return promptly (a slow one delays
// heartbeats) and must not wait for another method of this session; answer
// a tool.request from another goroutine. A handle of another attempt, in
// an event or the result, ends the session (KindForeignHandle).
func (s *Session) Run(ctx context.Context, p types.AdapterRunParams, onEvent func(Event)) (types.AdapterRunResult, error) {
	r := &run{want: stamp{Attempt: p.Attempt, FencingToken: p.FencingToken, Call: p.Call}, onEvent: onEvent,
		check: func(raw json.RawMessage) *Error { return foreignHandle(raw, p.Attempt) }}
	decode := result[types.AdapterRunResult]("run", "adapter/run-result")
	return typed[types.AdapterRunResult](s.call(ctx, "run", "adapter/run-params", p, r, expect{
		admit: s.fenced(p.Attempt, p.FencingToken, "", p),
		check: func(raw json.RawMessage) (any, *Error) {
			v, e := decode(raw)
			if e == nil {
				e = foreignHandle(raw, p.Attempt)
			}
			return v, e
		},
	}))
}

// ToolResponse answers a tool.request of the pending run; the adapter
// acknowledges with an empty object.
func (s *Session) ToolResponse(ctx context.Context, p types.AdapterToolResponseParams) error {
	_, err := s.call(ctx, "tool.response", "adapter/tool-response-params", p, nil, expect{
		admit: s.fenced(p.Attempt, p.FencingToken, p.Call, p),
		check: func(raw json.RawMessage) (any, *Error) {
			var ack map[string]json.RawMessage
			if json.Unmarshal(raw, &ack) != nil || ack == nil || len(ack) != 0 {
				return nil, &Error{Kind: KindProtocol, Method: "tool.response", Detail: "the acknowledgement is not an empty object"}
			}
			return nil, nil
		},
	})
	return err
}

// Cancel asks the adapter to stop the run: best effort, never relied on (09).
func (s *Session) Cancel(ctx context.Context, p types.AdapterFencedParams) (types.AdapterOutcome, error) {
	return typed[types.AdapterOutcome](s.call(ctx, "cancel", "adapter/fenced-params", p, nil, expect{
		admit: s.fenced(p.Attempt, p.FencingToken, "", p),
		check: result[types.AdapterOutcome]("cancel", "adapter/outcome"),
	}))
}

// Shutdown asks the adapter to end; its output ending afterwards is the
// session's clean end. Wait for the process with Wait.
func (s *Session) Shutdown(ctx context.Context) (types.AdapterOutcome, error) {
	o, err := typed[types.AdapterOutcome](s.call(ctx, "shutdown", "", nil, nil, expect{check: result[types.AdapterOutcome]("shutdown", "adapter/outcome")}))
	if err == nil {
		s.p.Stdin().Close()
	}
	return o, err
}

// fenced is the admission of the core's own params, run under the session's
// lock: the accepted prepare's attempt and token (and the pending run's
// call, when call is set), and handles of that attempt only.
func (s *Session) fenced(attempt string, token int64, call string, params any) func() error {
	b, err := json.Marshal(params)
	return func() error {
		p := s.fence.prepared
		switch {
		case p == nil:
			return ErrSequence
		case err != nil, p.Attempt != attempt, p.FencingToken != token, foreign(b, attempt):
			return ErrForeignParams
		case call != "" && (s.run == nil || s.run.want.Call != call):
			return ErrForeignParams
		}
		return nil
	}
}

// foreignHandle refuses a document holding a handle of another attempt.
func foreignHandle(doc json.RawMessage, own string) *Error {
	if !foreign(doc, own) {
		return nil
	}
	return &Error{Kind: KindForeignHandle, Method: "run", Check: CheckForeignHandle, Detail: "a handle of another attempt than the session's"}
}

// foreign reports whether doc holds, at any depth, a handle (an object with
// attempt, handle, digest and size) naming another attempt than own. Every
// handle is found whatever field carries it, so a schema adding one cannot
// slip past.
func foreign(doc json.RawMessage, own string) bool {
	var v any
	if json.Unmarshal(doc, &v) != nil {
		return true // never accepted unread
	}
	return foreignIn(v, own)
}

func foreignIn(v any, own string) bool {
	switch x := v.(type) {
	case map[string]any:
		_, h := x["handle"]
		_, d := x["digest"]
		_, z := x["size"]
		if a, ok := x["attempt"]; ok && h && d && z && a != own {
			return true
		}
		for _, e := range x {
			if foreignIn(e, own) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if foreignIn(e, own) {
				return true
			}
		}
	}
	return false
}
