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
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
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
	if len(args) >= 2 && args[0] == "dev" && args[1] == "capabilities" {
		return devCapabilities(args[2:], stdout, stderr)
	}
	if len(args) >= 2 && args[0] == "dev" && args[1] == "adapter-digest" {
		return devAdapterDigest(args[2:], stdout, stderr)
	}
	if len(args) >= 3 && args[0] == "dev" && args[1] == "bench" && args[2] == "record" {
		return benchRecord(ctx, args[3:], stdout, stderr, c)
	}
	if len(args) >= 1 && args[0] == "events" {
		return events(ctx, args[1:], stdout, stderr, c)
	}
	if len(args) >= 1 && args[0] == "board" {
		return read(ctx, "adc board", "board", args[1:], stdout, stderr, c)
	}
	if len(args) >= 1 && args[0] == "doctor" {
		return doctor(ctx, args[1:], stdout, stderr, c, hostSystem())
	}
	if len(args) >= 2 && args[0] == "blob" && args[1] == "put" {
		return blob(ctx, "adc blob put", true, args[2:], stdout, stderr, c)
	}
	if len(args) >= 3 && args[0] == "dev" && args[1] == "sandbox" && args[2] == "probe" {
		return sandboxProbe(ctx, args[3:], stdout, stderr, c)
	}
	if len(args) >= 3 && args[0] == "dev" && args[1] == "blob" && args[2] == "uploads" {
		return blob(ctx, "adc dev blob uploads", false, args[3:], stdout, stderr, c)
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
	if args[0] == "task" && args[1] == "show" {
		return taskShow(ctx, args[2:], stdout, stderr, c)
	}
	if args[0] == "task" && args[1] == "wait" {
		return taskWait(ctx, args[2:], stdout, stderr, c)
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
	reason := fs.String("reason", "", "why (required); task cancel also records it in its scope, whose own reason, if given, must equal it")
	scope := fs.String("scope", "{}", "the action's parameters, a JSON object")
	key := fs.String("idempotency-key", "", "repeat a command safely with the same key (default: a new key)")
	asJSON := fs.Bool("json", false, "print the response as one JSON document")
	tokenOut := fs.String("token-out", "", "a command that issues a token: the new file it is written to, 0600 (required; never an existing file)")
	scopeFile := fs.String("scope-file", "", "read the scope from this JSON file instead of --scope")
	var attach []string
	fs.Func("attach", "upload this file (at most 1 MiB) first and list it in the scope's attachments, as {name, digest}; repeatable",
		func(s string) error { attach = append(attach, s); return nil })
	if code, ok := parse(fs, args, stderr); !ok {
		return code
	}
	usage := func(format string, a ...any) int {
		say(stderr, "%s: %s\n", name, fmt.Sprintf(format, a...))
		return entry.ExitUsage
	}
	if *scopeFile != "" {
		b, err := os.ReadFile(*scopeFile)
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "scope" {
				err = errors.New("--scope is given too")
			}
		})
		if err != nil {
			return usage("--scope-file needs a readable file and no --scope")
		}
		*scope = string(b)
	}
	var obj map[string]json.RawMessage
	if len(attach) > 0 && (json.Unmarshal([]byte(*scope), &obj) != nil || obj == nil || obj["attachments"] != nil) {
		return usage("--attach needs a scope that is a JSON object without attachments")
	}
	names := map[string]bool{}
	for _, f := range attach {
		if names[filepath.Base(f)] {
			return usage("--attach names %s twice", filepath.Base(f))
		}
		names[filepath.Base(f)] = true
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
	if len(attach) > 0 {
		list := make([]map[string]string, 0, len(attach))
		for _, f := range attach {
			u, _, err := put(ctx, c, f)
			if err != nil {
				return failed(stderr, name+" --attach", err) // nothing was sent
			}
			list = append(list, map[string]string{"name": filepath.Base(f), "digest": string(u.Digest)})
		}
		b, err := json.Marshal(list)
		if err == nil {
			obj["attachments"] = b
			b, err = json.Marshal(obj)
		}
		if err != nil {
			return usage("--attach: %v", err)
		}
		*scope = string(b)
	}
	// task.cancel records its reason in its scope, since the envelope's is
	// not recorded with commands yet (#147): --reason fills a scope that
	// names none (Opus #298 L5). The scope is read strictly first, so a
	// duplicate key, literal or escaped, is refused rather than resolved by
	// the re-encoding (Astra #314 M1); a scope reason that differs from
	// --reason is refused, never silently preferred (L2).
	if action == "task.cancel" {
		doc, err := contract.ReadStrict([]byte(*scope))
		if err != nil {
			return usage("--scope: %v", err)
		}
		if o, ok := doc.(map[string]any); ok {
			switch r, named := o["reason"]; {
			case named && r != *reason:
				return usage("--reason %q and the scope's reason differ: give one, or the same", *reason)
			case !named:
				var raw map[string]json.RawMessage
				b, err := json.Marshal(*reason)
				if err == nil {
					err = json.Unmarshal([]byte(*scope), &raw)
				}
				if err == nil {
					raw["reason"] = b
					b, err = json.Marshal(raw)
				}
				if err != nil {
					return usage("--reason: %v", err)
				}
				*scope = string(b)
			}
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
		if v.Policy != types.CommandProposedPolicyHuman {
			if err := say(w, "proposed: %s needs an independent review (binding %s)\n", v.Proposal, v.BindingDigest); err != nil {
				return err
			}
			return sayEffective(w, v.EffectiveChange)
		}
		// A human confirmation cannot be completed from the CLI (D37).
		if err := say(w, "proposed: %s needs a human confirmation on the dashboard (binding %s)\n  requested: %s %s\n",
			v.Proposal, v.BindingDigest, action, string(scope)); err != nil {
			return err
		}
		if err := sayEffective(w, v.EffectiveChange); err != nil {
			return err
		}
		return say(w, "  confirm at: %s%s\n", DashboardProposal, v.Proposal)
	case *types.CommandRefused:
		return say(w, "refused: %s/%s: %s\n", v.Outcome, v.Reason, v.Detail)
	}
	return say(w, "no response\n")
}

// sayEffective writes a proposal's effective change: what it binds, as
// authority derived it, which the request may not name (04, 08 "CLI
// rules"; a grant override decides the policy, not the change).
func sayEffective(w io.Writer, e types.EffectiveChange) error {
	version, project, grantee := "its digest", "installation-wide", "none"
	if e.Object.Version != nil {
		version = "version " + strconv.FormatInt(*e.Object.Version, 10)
	}
	if e.Project != nil {
		project = string(*e.Project)
	}
	if e.Grantee != nil {
		grantee = string(*e.Grantee)
	}
	classes := make([]string, 0, len(e.ChangeClass))
	for _, c := range e.ChangeClass {
		classes = append(classes, string(c))
	}
	if len(classes) == 0 {
		classes = append(classes, "none")
	}
	return say(w, "  effective: %s on %s %s at %s; project %s; change classes %s; grantee %s\n",
		e.Action, e.Object.Type, e.Object.ID, version, project, strings.Join(classes, ", "), grantee)
}

// NoTransport is the Client of a build with no controller transport yet.
type NoTransport struct{}

// Command always fails with ErrNoTransport.
func (NoTransport) Command(context.Context, string, []byte) ([]byte, error) {
	return nil, ErrNoTransport
}

// Uploader is a Client that also uploads a blob (POST /v1/blobs), or with
// nil data lists the uploads.
type Uploader interface {
	Blob(ctx context.Context, data []byte) ([]byte, error)
}

// put uploads the file at path; the server refuses one over its bound, so
// no more than one byte past it is read or sent.
func put(ctx context.Context, c Client, path string) (types.BlobUpload, []byte, error) {
	var u types.BlobUpload
	up, ok := c.(Uploader)
	if !ok {
		return u, nil, ErrNoTransport
	}
	f, err := os.Open(path)
	if err != nil {
		return u, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, blobstore.MaxUploadBytes+1))
	if err = errors.Join(err, f.Close()); err != nil {
		return u, nil, err
	}
	raw, err := up.Blob(ctx, append([]byte{}, data...))
	if err == nil {
		err = json.Unmarshal(raw, &u)
	}
	return u, raw, err
}

// blob is `adc blob put --file F` (isPut) and `adc dev blob uploads`.
func blob(ctx context.Context, name string, isPut bool, args []string, stdout, stderr io.Writer, c Client) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the controller's response as one JSON document")
	file := new(string)
	if isPut {
		file = fs.String("file", "", "the file to upload, at most 1 MiB (required)")
	}
	if code, ok := parse(fs, args, stderr); !ok {
		return code
	}
	if fs.NArg() > 0 || (isPut && *file == "") {
		say(stderr, "%s: unexpected argument, or no --file\n", name)
		return entry.ExitUsage
	}
	var raw []byte
	var err error
	if isPut {
		_, raw, err = put(ctx, c, *file)
	} else if up, ok := c.(Uploader); ok {
		raw, err = up.Blob(ctx, nil)
	} else {
		err = ErrNoTransport
	}
	if err != nil {
		return failed(stderr, name, err)
	}
	var list types.ReadList
	if *asJSON {
		err = sayJSON(stdout, raw)
	} else if !isPut {
		err = json.Unmarshal(raw, &list)
	} else {
		list.Items = []json.RawMessage{raw}
	}
	for _, it := range list.Items {
		var u types.BlobUpload
		if err = errors.Join(err, json.Unmarshal(it, &u)); err == nil {
			err = say(stdout, "%s by %s: %d bytes, expires at %s\n", u.Digest, u.Actor, u.Size, u.ExpiresAt)
		}
	}
	if err != nil {
		say(stderr, "%s: could_not_run: writing the result: %s\n", name, err)
		return entry.ExitCouldNotRun
	}
	return entry.ExitOK
}
