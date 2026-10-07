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
	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Client sends one command body (validated CommandRequest JSON, sent as
// is) to the controller and returns its raw response body. The transport
// (the Unix socket or HTTPS, 08 "Transport") is the caller's.
type Client interface {
	Command(ctx context.Context, action string, body []byte) ([]byte, error)
}

// ErrNoTransport: this build has no controller transport yet.
var ErrNoTransport = errors.New("no controller transport in this build")

// Main runs adc's command line and returns the exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer, c Client) int {
	if len(args) >= 2 && args[0] == "dev" && args[1] == "fixtures" {
		return devFixtures(ctx, args[2:], stdout, stderr)
	}
	if len(args) >= 2 && args[0] == "dev" && args[1] == "verify-log" {
		return verifyLog(ctx, args[2:], stdout, stderr)
	}
	if len(args) == 0 || !isNoun(args[0]) {
		return entry.Main("adc", args, stdout, stderr)
	}
	if len(args) < 2 {
		say(stderr, "adc %s: missing verb\n", args[0])
		return entry.ExitUsage
	}
	if args[0] == "admin" && args[1] == "bootstrap" {
		return adminBootstrap(ctx, args[2:], stdout, stderr, c)
	}
	if resource, ok := reads[command{args[0], args[1]}]; ok {
		return read(ctx, "adc "+args[0]+" "+args[1], resource, args[2:], stdout, stderr, c)
	}
	action, ok := actionFor(args[0], args[1])
	if !ok {
		say(stderr, "adc %s: unknown verb %s\n", args[0], args[1])
		return entry.ExitUsage
	}
	return run(ctx, "adc "+args[0]+" "+args[1], action, args[2:], stdout, stderr, c)
}

func run(ctx context.Context, name, action string, args []string, stdout, stderr io.Writer, c Client) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	expect := fs.String("expect", "", "the exact revision inspected: a version (0: the object does not exist yet) or a sha256: digest (required)")
	reason := fs.String("reason", "", "why (required)")
	scope := fs.String("scope", "{}", "the action's parameters, a JSON object")
	key := fs.String("idempotency-key", "", "repeat a command safely with the same key (default: a new key)")
	asJSON := fs.Bool("json", false, "print the response as one JSON document")
	tokenOut := fs.String("token-out", "", "a command that issues a token: the new file it is written to, 0600 (required; never an existing file)")
	if code, ok := parse(fs, args, stderr); !ok {
		return code
	}
	usage := func(format string, a ...any) int {
		say(stderr, "%s: %s\n", name, fmt.Sprintf(format, a...))
		return entry.ExitUsage
	}
	if fs.NArg() > 0 {
		return usage("unexpected argument %s", fs.Arg(0))
	}
	recovery, issues := issuing[action]
	if issues != (*tokenOut != "") {
		return usage("--token-out FILE is required by a command that issues a token, and taken by no other")
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
		if *key, err = newKey(); err != nil {
			say(stderr, "%s: could_not_run: %s\n", name, err)
			return entry.ExitCouldNotRun
		}
	}
	req := types.CommandRequest{IdempotencyKey: *key, Expected: expected, Scope: json.RawMessage(*scope), Reason: *reason}
	// The bytes validated are the bytes sent.
	body, err := contract.Encode("api/command-request", req)
	if err != nil {
		return usage("invalid command: %v", err)
	}
	var out *client.TokenOut
	if issues {
		if out, err = client.CreateTokenOut(*tokenOut); err != nil {
			if errors.Is(err, client.ErrTokenOutExists) || errors.Is(err, client.ErrTokenOutDir) || errors.Is(err, client.ErrTokenOutOwn) {
				return usage("%v", err)
			}
			say(stderr, "%s: could_not_run: %s; nothing was sent\n", name, err)
			return entry.ExitCouldNotRun
		}
		defer out.Discard()
	}
	// Once sent, the command may have run: a retry must reuse its key.
	lost := ""
	if issues {
		lost = "; if it applied, a repeat delivers no token: " + recovery
	}
	couldNotRun := func(format string, a ...any) int {
		say(stderr, "%s: could_not_run: %s; to retry safely, repeat it with --idempotency-key %s%s\n", name, fmt.Sprintf(format, a...), *key, lost)
		return entry.ExitCouldNotRun
	}
	raw, err := c.Command(ctx, action, body)
	switch {
	case errors.Is(err, client.ErrUnauthenticated):
		say(stderr, "%s: refused: %s\n", name, err)
		return entry.ExitRefused
	case errors.Is(err, client.ErrBadRequest):
		return usage("%v", err)
	case err != nil:
		return couldNotRun("%v", err)
	}
	// Fixed text: the response is never quoted (it may hold a credential).
	resp, err := client.DecodeResponse(raw)
	if err != nil {
		return couldNotRun("%v", client.ErrInvalidResponse)
	}
	// The schema cannot say which outcomes a reason accompanies; the registry can.
	if v, ok := resp.Variant.(*types.CommandRefused); ok {
		reg, err := vocab.Load()
		if err != nil {
			return couldNotRun("the vocabulary: %v", err)
		}
		if err := reg.CheckRead(vocab.Result{Outcome: vocab.Outcome(v.Outcome), Reason: v.Reason, Detail: v.Detail}); err != nil {
			return couldNotRun("the controller's refusal is invalid: %v", err)
		}
	}
	// An issued token goes to --token-out alone, never to stdout.
	if v, ok := resp.Variant.(*types.CommandApplied); ok {
		clean, code, done := deliver(name, resp, v, out, recovery, stderr)
		if done {
			return code
		}
		if clean != nil {
			raw = clean
			defer say(stderr, "%s: the issued token was written to %s\n", name, *tokenOut)
		}
	}
	if *asJSON {
		err = sayJSON(stdout, raw)
	} else {
		err = describe(stdout, resp, action, req.Scope)
	}
	if err != nil {
		say(stderr, "%s: writing the result: %s\n", name, err)
	}
	return ExitCode(resp)
}

// newKey is a fresh idempotency key.
func newKey() (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	return "cli-" + hex.EncodeToString(b), err
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
		return types.ExpectedState{}, fmt.Errorf("--expect %s is neither a version nor a sha256: digest", s)
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

// DashboardProposal is where the dashboard shows a proposal: a path
// placeholder until the dashboard ships (M5).
const DashboardProposal = "/dashboard/proposals/"

// describe writes a response's human lines, every value through say.
func describe(w io.Writer, r types.CommandResponse, action string, scope json.RawMessage) error {
	switch v := r.Variant.(type) {
	case *types.CommandApplied:
		repeat := ""
		if v.Replayed {
			repeat = " (a repeat; nothing ran again)"
		}
		return say(w, "applied: %s%s\n", v.Command, repeat)
	case *types.CommandProposed:
		if v.Policy == types.CommandProposedPolicyHuman {
			// A human confirmation cannot be completed from the CLI (D37).
			return say(w, "proposed: %s needs a human confirmation on the dashboard (binding %s)\n  requested: %s %s\n  confirm at: %s%s\n",
				v.Proposal, v.BindingDigest, action, string(scope), DashboardProposal, v.Proposal)
		}
		return say(w, "proposed: %s needs an independent review (binding %s)\n", v.Proposal, v.BindingDigest)
	case *types.CommandRefused:
		return say(w, "refused: %s/%s: %s\n", v.Outcome, v.Reason, v.Detail)
	}
	return say(w, "no response\n")
}

// NoTransport is the Client of a build with no controller transport yet.
type NoTransport struct{}

// Command always fails with ErrNoTransport.
func (NoTransport) Command(context.Context, string, []byte) ([]byte, error) {
	return nil, ErrNoTransport
}
