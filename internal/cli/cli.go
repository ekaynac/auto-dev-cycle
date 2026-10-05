// SPDX-License-Identifier: Apache-2.0

// Package cli is adc's command tree (docs/design/m0/08 "CLI rules"): one
// command per action, which builds the command envelope
// (schemas/api/command-request.json), sends it through a Client, and maps
// the response to the single exit-code table. Everything else falls through
// to internal/entry (version, dev).
package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Client sends one command to the controller and returns its raw response
// body. The transport (the Unix socket or HTTPS, 08 "Transport") is the
// caller's.
type Client interface {
	Command(ctx context.Context, action string, req types.CommandRequest) ([]byte, error)
}

// ErrNoTransport: this build has no controller transport yet.
var ErrNoTransport = errors.New("no controller transport in this build")

// Main runs adc's command line and returns the exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer, c Client) int {
	if len(args) == 0 || !isNoun(args[0]) {
		return entry.Main("adc", args, stdout, stderr)
	}
	if len(args) < 2 {
		fmt.Fprintf(stderr, "adc %s: missing verb\n", args[0])
		return entry.ExitUsage
	}
	action, ok := actionFor(args[0], args[1])
	if !ok {
		fmt.Fprintf(stderr, "adc %s: unknown verb %q\n", args[0], args[1])
		return entry.ExitUsage
	}
	return run(ctx, "adc "+args[0]+" "+args[1], action, args[2:], stdout, stderr, c)
}

func run(ctx context.Context, name, action string, args []string, stdout, stderr io.Writer, c Client) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	expect := fs.String("expect", "", "the exact revision inspected: a version (0: the object does not exist yet) or a sha256: digest (required)")
	reason := fs.String("reason", "", "why (required)")
	scope := fs.String("scope", "{}", "the action's parameters, a JSON object")
	key := fs.String("idempotency-key", "", "repeat a command safely with the same key (default: a new key)")
	asJSON := fs.Bool("json", false, "print the response as one JSON document")
	if err := fs.Parse(args); err != nil {
		return entry.ExitUsage // the flag package printed why
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, name+": "+format+"\n", a...)
		return entry.ExitUsage
	}
	if fs.NArg() > 0 {
		return usage("unexpected argument %q", fs.Arg(0))
	}
	// The revision is never fetched for the caller [M13].
	if *expect == "" {
		return usage("--expect is required")
	}
	expected, err := parseExpect(*expect)
	if err != nil {
		return usage("%v", err)
	}
	if *key == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			fmt.Fprintf(stderr, "%s: could_not_run: %v\n", name, err)
			return entry.ExitCouldNotRun
		}
		*key = "cli-" + hex.EncodeToString(b)
	}
	req := types.CommandRequest{IdempotencyKey: *key, Expected: expected, Scope: json.RawMessage(*scope), Reason: *reason}
	body, err := json.Marshal(req)
	if err != nil {
		return usage("--scope: %v", err)
	}
	if err := contract.Validate("api/command-request", body); err != nil {
		return usage("invalid command: %v", err)
	}
	raw, err := c.Command(ctx, action, req)
	if err != nil {
		fmt.Fprintf(stderr, "%s: could_not_run: %v\n", name, err)
		return entry.ExitCouldNotRun
	}
	var resp types.CommandResponse
	if err := contract.Validate("api/command-response", raw); err != nil {
		fmt.Fprintf(stderr, "%s: could_not_run: the controller's response is invalid: %v\n", name, err)
		return entry.ExitCouldNotRun
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		fmt.Fprintf(stderr, "%s: could_not_run: the controller's response is invalid: %v\n", name, err)
		return entry.ExitCouldNotRun
	}
	if *asJSON {
		_, err = fmt.Fprintf(stdout, "%s\n", raw)
	} else {
		_, err = fmt.Fprintln(stdout, describe(resp))
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: writing the result: %v\n", name, err)
	}
	return ExitCode(resp)
}

// parseExpect reads --expect: a version or a digest, never neither; the
// request schema checks the value.
func parseExpect(s string) (types.ExpectedState, error) {
	if strings.HasPrefix(s, "sha256:") {
		d := types.Digest(s)
		return types.ExpectedState{Digest: &d}, nil
	}
	v, err := strconv.ParseInt(s, 10, 64) // a negative one is the schema's to refuse
	if err != nil {
		return types.ExpectedState{}, fmt.Errorf("--expect %q is neither a version nor a sha256: digest", s)
	}
	return types.ExpectedState{Version: &v}, nil
}

// ExitCode maps a response to the exit-code table (08 "CLI rules").
func ExitCode(r types.CommandResponse) int {
	switch v := r.Variant.(type) {
	case *types.CommandApplied:
		return entry.ExitOK
	case *types.CommandProposed:
		return entry.ExitProposed
	case *types.CommandRefused:
		switch {
		case v.Outcome == types.CommandRefusedOutcomeCouldNotRun:
			return entry.ExitCouldNotRun
		case v.Reason == vocab.ReasonStaleExpectedState, v.Reason == vocab.ReasonStaleAttempt, v.Reason == vocab.ReasonIdempotencyKeyReused:
			return entry.ExitStale
		}
		return entry.ExitRefused
	}
	return entry.ExitCouldNotRun
}

func describe(r types.CommandResponse) string {
	switch v := r.Variant.(type) {
	case *types.CommandApplied:
		s := fmt.Sprintf("applied: %s", v.Command)
		if v.Replayed {
			s += " (a repeat; nothing ran again)"
		}
		return s
	case *types.CommandProposed:
		if v.Policy == types.CommandProposedPolicyHuman {
			// A human confirmation cannot be completed from the CLI (D37).
			return fmt.Sprintf("proposed: %s needs a human confirmation on the dashboard (binding %s)", v.Proposal, v.BindingDigest)
		}
		return fmt.Sprintf("proposed: %s needs an independent review (binding %s)", v.Proposal, v.BindingDigest)
	case *types.CommandRefused:
		return fmt.Sprintf("refused: %s/%s: %s", v.Outcome, v.Reason, v.Detail)
	}
	return "no response"
}

// NoTransport is the Client of a build with no controller transport yet.
type NoTransport struct{}

// Command always fails with ErrNoTransport.
func (NoTransport) Command(context.Context, string, types.CommandRequest) ([]byte, error) {
	return nil, ErrNoTransport
}
