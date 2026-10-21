// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session/inproc"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/wire"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// outputName is the handle every attempt of the fixture declares.
const outputName = "patch.diff"

// peerDescribe is the scripted peer's describe result.
const peerDescribe = `{"harness":"fixture-peer","harness_version":"1.0.0","adapter_version":"0.1.0","contract_version":"1.0.0",` +
	`"capabilities":["patch"],"session_semantics":"fresh_process","tool_surface":[],` +
	`"telemetry":{"usage":true,"upstream":false,"stream_deltas":false,"per_turn_coverage":false},"host_side":false,"needs_native_login":false}`

const peerClean = `{"outcome":"empty","reason":"completed_clean","detail":""}`

// peerUsage is the one usage delta each run reports: what step 8 must find
// counted for the stale attempt.
const peerUsageIn = 1200

// declared is a handle of attempt for content, with its digest and size.
func declared(attempt types.AttemptID, content string) types.AdapterHandle {
	sum := sha256.Sum256([]byte(content))
	return types.AdapterHandle{Attempt: attempt, Handle: outputName, Digest: types.Digest("sha256:" + hex.EncodeToString(sum[:])), Size: types.Amount(len(content))}
}

// unsafeCase is one attempt: write makes its output in out/ (as the
// adapter, before it answers run) and returns the handle it declares;
// beforeResult runs just before the result is sent.
type unsafeCase struct {
	name         string
	check        types.AttemptOutputRefusalCheck // the refusal wanted, "" for a clean submission
	write        func(out string, att types.AttemptID) (types.AdapterHandle, error)
	beforeResult func(att types.AttemptID) error
}

// contentOf is a case's own patch: no two cases publish the same bytes.
func contentOf(name string) string {
	return "--- a/f\n+++ b/f\n@@ -1 +1 @@\n-old\n+" + name + "\n"
}

// plainOutput writes the case's patch as a regular file and declares it.
func plainOutput(name string) func(string, types.AttemptID) (types.AdapterHandle, error) {
	return func(out string, att types.AttemptID) (types.AdapterHandle, error) {
		c := contentOf(name)
		return declared(att, c), os.WriteFile(filepath.Join(out, outputName), []byte(c), 0o644)
	}
}

// scriptedPeer is the adapter of one attempt, in this process over the
// session's pipes: it answers describe, prepare and shutdown cleanly, and
// run with the case's output, one usage delta and a found result. Its own
// failure (a file it could not make) lands in *failed.
func scriptedPeer(c unsafeCase, failed *error) func(io.Reader, io.Writer) {
	return peer(func(p types.AdapterPrepareParams, params json.RawMessage, send func(any) error) (json.RawMessage, error) {
		return peerRun(c, p.Exchange.Out, params, send)
	}, failed)
}

// peer is a scripted adapter: describe, prepare and shutdown are answered
// cleanly, run by run with prepare's params.
func peer(run func(types.AdapterPrepareParams, json.RawMessage, func(any) error) (json.RawMessage, error), failed *error) func(io.Reader, io.Writer) {
	return func(in io.Reader, out io.Writer) {
		r, w := wire.NewReader(in), wire.NewWriter(out)
		var prepared types.AdapterPrepareParams
		send := func(v any) error {
			b, err := wire.Encode(v)
			if err == nil {
				err = w.Write(b)
			}
			return err
		}
		for {
			line, err := r.Next()
			if err != nil {
				return // the core closed the session
			}
			var req wire.Request
			if err := json.Unmarshal(line, &req); err != nil {
				*failed = err
				return
			}
			result := json.RawMessage(peerClean)
			switch req.Method {
			case "describe":
				result = json.RawMessage(peerDescribe)
			case "prepare":
				if err := json.Unmarshal(req.Params, &prepared); err != nil {
					*failed = err
					return
				}
			case "run":
				if result, err = run(prepared, req.Params, send); err != nil {
					*failed = err
					return
				}
			}
			if send(map[string]any{"jsonrpc": wire.Version, "id": req.ID, "result": result}) != nil {
				return
			}
		}
	}
}

// peerRun makes the case's output, reports usage and returns the result.
func peerRun(c unsafeCase, outDir string, params json.RawMessage, send func(any) error) (json.RawMessage, error) {
	var p types.AdapterRunParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	h, err := c.write(outDir, p.Attempt)
	if err != nil {
		return nil, fmt.Errorf("%s: making the output: %w", c.name, err)
	}
	usage := json.RawMessage(fmt.Sprintf(`{"event":"usage","attempt":%q,"fencing_token":%d,"call":%q,"sub_call":"s1","sequence":0,`+
		`"tokens":{"in":%d,"out":120,"cache_read":0,"cache_write":0},"turns":1,"milliseconds":900,`+
		`"cost":{"amount_micros":1320,"currency":"USD","basis":"cash","estimate":true}}`, p.Attempt, p.FencingToken, p.Call, peerUsageIn))
	if err := send(map[string]any{"jsonrpc": wire.Version, "method": wire.EventMethod, "params": usage}); err != nil {
		return nil, err
	}
	if c.beforeResult != nil {
		if err := c.beforeResult(p.Attempt); err != nil {
			return nil, fmt.Errorf("%s: before the result: %w", c.name, err)
		}
	}
	return json.Marshal(types.AdapterRunResult{Outcome: types.OutcomeFound, Reason: vocab.ReasonCompletedWithFindings,
		Detail: "the fixture's patch", Result: &types.AdapterRunResultResultValue{Kind: types.AdapterRunResultResultValueKindPatch, Handle: h}})
}

// unsafeAdapterOutputs: an adapter's outputs reach the blob store only
// through the ingestion boundary. A clean output is published; a symlink
// (to an outside file whose bytes match the declared digest), a FIFO, a
// hard link, a digest mismatch and a handle of another attempt are each
// refused unsafe_output, an output of an attempt stopped while it ran is
// refused stale_attempt, and that attempt's usage is still counted (11
// "unsafe adapter outputs"; 09 "Blob exchange: the ingestion boundary"; 05).
// The adapter is a scripted peer in this process speaking the protocol to
// the real host over pipes (session.Config.InProcess, package inproc): the installed check has
// neither bubblewrap nor node to spawn one.
func unsafeAdapterOutputs(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	reg, err := vocab.Load()
	if err != nil {
		return nil, nil, err
	}
	state, outside, root := filepath.Join(dir, "state"), filepath.Join(dir, "outside"), filepath.Join(dir, "exchange")
	for _, d := range []string{outside, root} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, nil, err
		}
	}
	lock, err := eventstore.AcquireLock(state)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, lock.Release()) }()
	blobs, err := blobstore.Open(state)
	if err != nil {
		return nil, nil, err
	}
	kinds := lease.Kinds()
	maps.Copy(kinds, ledger.Kinds())
	project := lease.Projector{Blobs: blobs, Holds: reg.Holds, Units: oneTask{}}.Project
	refusals := map[string]types.AttemptOutputRefusal{} // the attempt.output_refused events appended, by event id
	s, err := eventstore.Open(ctx, lock, eventstore.Config{Kinds: kinds, Blobs: blobs,
		Hooks: eventstore.Hooks{Identify: func(*eventstore.Tx, eventstore.Command) error { return nil }, Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil },
			Project: func(tx *eventstore.Tx, evs []eventstore.Event) error {
				for _, ev := range evs {
					if ev.Kind == lease.OutputRefused {
						var r types.AttemptOutputRefusal
						if err := json.Unmarshal(ev.Payload, &r); err != nil {
							return err
						}
						refusals[ev.ID] = r
					}
				}
				return errors.Join(project(tx, evs), ledger.Project(tx, evs))
			}}})
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	actor, err := ids.New("act")
	if err != nil {
		return nil, nil, err
	}
	do := func(fn func(*eventstore.Tx) ([]eventstore.Event, error)) error {
		cmd, err := ids.New("cmd")
		if err != nil {
			return err
		}
		_, err = s.SubmitApplied(ctx, eventstore.Command{ID: cmd, Actor: actor, Action: "fixture.command", IdempotencyKey: cmd, Input: []byte(`{}`)}, eventstore.Applies(
			func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				evs, err := fn(tx)
				return evs, json.RawMessage(`{}`), err
			}))
		return err
	}
	staging := filepath.Join(state, "blobs", "tmp") // blobstore.Stage's directory
	host := &adapterhost.Host{Store: s, Blobs: blobs, Actor: actor, Root: root}

	// The outside files: the sentinel a symlink names, and a file a hard
	// link joins. Neither may change, nor reach the blob store.
	sentinel, linked := filepath.Join(outside, "sentinel"), filepath.Join(outside, "linked")
	sentinelBytes := contentOf("sentinel")
	for _, f := range []string{sentinel, linked} {
		if err := os.WriteFile(f, []byte(contentOf(filepath.Base(f))), 0o600); err != nil {
			return nil, nil, err
		}
	}
	sentinelBefore, err := os.Lstat(sentinel)
	if err != nil {
		return nil, nil, err
	}
	stop := func(att types.AttemptID) error {
		return do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.ChangeAttempt(tx, att, 1, lease.AttemptEvent{Kind: lease.StopRequested, StopReason: types.AttemptStopReasonValueRevoke})
		})
	}
	cases := []unsafeCase{
		{name: "clean", write: plainOutput("clean")},
		{name: "symlink", check: types.AttemptOutputRefusalCheckSymlink,
			write: func(out string, att types.AttemptID) (types.AdapterHandle, error) {
				return declared(att, sentinelBytes), os.Symlink(sentinel, filepath.Join(out, outputName))
			}},
		{name: "fifo", check: types.AttemptOutputRefusalCheckSpecialFile,
			write: func(out string, att types.AttemptID) (types.AdapterHandle, error) {
				return declared(att, contentOf("fifo")), syscall.Mkfifo(filepath.Join(out, outputName), 0o644)
			}},
		{name: "hard-link", check: types.AttemptOutputRefusalCheckHardLink,
			write: func(out string, att types.AttemptID) (types.AdapterHandle, error) {
				return declared(att, contentOf("linked")), os.Link(linked, filepath.Join(out, outputName))
			}},
		{name: "digest", check: types.AttemptOutputRefusalCheckDigest,
			write: func(out string, att types.AttemptID) (types.AdapterHandle, error) {
				h, err := plainOutput("digest")(out, att)
				h.Digest = declared(att, contentOf("tsegid")).Digest // other bytes, the same size
				return h, err
			}},
		{name: "foreign-handle", check: types.AttemptOutputRefusalCheckForeignHandle,
			write: func(out string, att types.AttemptID) (types.AdapterHandle, error) {
				other, err := ids.New("att")
				h, werr := plainOutput("foreign-handle")(out, att)
				h.Attempt = types.AttemptID(other)
				return h, errors.Join(err, werr)
			}},
		{name: "stale", check: types.AttemptOutputRefusalCheckStaleAttempt, write: plainOutput("stale"), beforeResult: stop},
	}

	var o observe
	var staleCall types.CallID
	for _, c := range cases {
		out, att, call, failed, err := runCase(ctx, host, do, reg.Holds, c)
		switch {
		case err != nil:
			return nil, nil, fmt.Errorf("%s: %w", c.name, err)
		case failed != nil:
			return nil, nil, fmt.Errorf("%s: the peer: %w", c.name, failed)
		}
		st, err := attemptStep(out)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", c.name, err)
		}
		o.steps = append(o.steps, st)
		if c.check == types.AttemptOutputRefusalCheckStaleAttempt {
			staleCall = call
		}
		if err := checkCase(ctx, &o, s, blobs, refusals, c, att, out); err != nil {
			return nil, nil, err
		}
		// Nothing staged outlives the attempt: a refused copy is discarded,
		// a submitted one published.
		left, err := os.ReadDir(staging)
		if err != nil {
			return nil, nil, err
		}
		o.check(len(left) == 0, "%s: %d staged copies left in the blob store's tmp/", c.name, len(left))
	}

	// The stale attempt's usage is counted whatever its output became.
	var ms []types.UsageMeasurement
	if err := s.View(ctx, func(tx *eventstore.Tx) (err error) { ms, err = ledger.Measurements(tx, staleCall); return err }); err != nil {
		return nil, nil, err
	}
	calls, err := ledger.Reconcile(ms)
	if err != nil {
		return nil, nil, err
	}
	got, ok := calls[staleCall]
	st := Step{vocab.Empty, vocab.ReasonCompletedClean.String()}
	if ok && got.Inconsistent {
		st = Step{vocab.Found, got.Reason.String()}
	}
	o.steps = append(o.steps, st)
	o.check(ok && got.Usage.In == peerUsageIn, "the stale attempt's usage is not counted: %+v", got)

	sentinelAfter, err := os.Lstat(sentinel)
	if err != nil {
		return nil, nil, err
	}
	now, err := os.ReadFile(sentinel)
	if err != nil {
		return nil, nil, err
	}
	o.check(bytes.Equal(now, []byte(sentinelBytes)) && sentinelAfter.ModTime().Equal(sentinelBefore.ModTime()) &&
		sentinelAfter.Mode() == sentinelBefore.Mode(), "the outside sentinel changed")
	return o.steps, o.violations, nil
}

// runCase grants a fresh lease and runs one attempt of c through the host,
// with a fresh exchange root; it returns the outcome, the attempt and its
// call, and the peer's own failure.
func runCase(ctx context.Context, host *adapterhost.Host, do func(func(*eventstore.Tx) ([]eventstore.Event, error)) error,
	holds map[string]types.Hold, c unsafeCase) (out adapterhost.Outcome, att types.AttemptID, call types.CallID, failed error, err error) {
	l, att, err := freshLease()
	cid, ierr := ids.New("cal")
	if err = errors.Join(err, ierr); err != nil {
		return out, "", "", nil, err
	}
	if err := do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, l) }); err != nil {
		return out, "", "", nil, err
	}
	if err := do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, l.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, holds)
	}); err != nil {
		return out, "", "", nil, err
	}
	call = types.CallID(cid)
	out, err = host.Run(ctx, adapterhost.Spec{AnyTool: true, Lease: l.ID, LeaseRevision: 2, Attempt: att, Call: call, Prompt: []byte("fix it\n"),
		Base:    types.AttemptBase{Tree: types.GitID("git:" + "cccccccccccccccccccccccccccccccccccccccc"), ContractRevision: "7"},
		Adapter: session.Config{InProcess: inproc.New(scriptedPeer(c, &failed)), Deadline: 30 * time.Second},
		Prepare: types.AdapterPrepareParams{Role: "builder", Executor: l.Executor, Model: "fixture-model",
			Workspace:  types.AdapterPrepareParamsWorkspace{Path: "/workspace", Writable: true},
			Limits:     types.AdapterPrepareParamsLimits{MaxTurns: 8, MaxOutputTokens: 8000, WallSeconds: 60},
			ToolPolicy: types.AdapterPrepareParamsToolPolicy{Allowed: []string{"Edit"}, Mediated: []string{}}}})
	return out, att, call, failed, err
}

// attemptStep maps an attempt's end to its step: a submission is
// empty/completed_clean, a refusal failed/<its reason>.
func attemptStep(out adapterhost.Outcome) (Step, error) {
	switch {
	case out.StartRefused != nil:
		return Step{}, out.StartRefused
	case len(out.Refusals) > 0:
		return Step{vocab.Failed, string(out.Refusals[0].Reason)}, nil
	case out.Submitted != nil:
		return Step{vocab.Empty, vocab.ReasonCompletedClean.String()}, nil
	}
	return Step{}, fmt.Errorf("the attempt ended with neither a submission nor a refusal: %v", out.Session)
}

// checkCase checks what one attempt committed: a clean one published its
// bytes and stays trustworthy; a refused one published neither its
// declared digest nor its file's bytes, has exactly one refusal event with
// the case's check and is untrustworthy.
func checkCase(ctx context.Context, o *observe, s *eventstore.Store, blobs *blobstore.Store,
	refusals map[string]types.AttemptOutputRefusal, c unsafeCase, attempt types.AttemptID, out adapterhost.Outcome) error {
	if out.Result == nil && c.check != types.AttemptOutputRefusalCheckForeignHandle {
		return fmt.Errorf("%s: no run result (%v)", c.name, out.Session)
	}
	var checks []types.AttemptOutputRefusalCheck
	for _, r := range refusals {
		if r.Attempt == attempt {
			checks = append(checks, r.Check)
		}
	}
	var untrustworthy bool
	if err := s.View(ctx, func(tx *eventstore.Tx) (err error) { untrustworthy, err = lease.Untrustworthy(tx, attempt); return err }); err != nil {
		return err
	}
	digests := []string{}
	if out.Result != nil && out.Result.Result != nil {
		digests = append(digests, string(out.Result.Result.Handle.Digest))
	}
	if c.check != types.AttemptOutputRefusalCheckSymlink {
		digests = append(digests, string(declared("", contentOf(c.name)).Digest))
	}
	if c.check == types.AttemptOutputRefusalCheckHardLink {
		digests = append(digests, string(declared("", contentOf("linked")).Digest))
	}
	for _, d := range digests {
		has, err := blobs.Has(d)
		if err != nil {
			return err
		}
		o.check(has == (c.check == ""), "%s: %s in the blob store is %t", c.name, d, has)
	}
	if c.check == "" {
		o.check(len(checks) == 0 && !untrustworthy, "%s: a clean output was refused %v (untrustworthy %t)", c.name, checks, untrustworthy)
		return nil
	}
	o.check(len(checks) == 1 && checks[0] == c.check, "%s: refusal events %v, want one %s", c.name, checks, c.check)
	o.check(untrustworthy, "%s: the attempt is not untrustworthy after a refusal", c.name)
	return nil
}
