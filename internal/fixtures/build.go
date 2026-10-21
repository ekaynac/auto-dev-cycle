// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session/inproc"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/wire"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab/exitmap"
)

// creditBeforeEdit: a 402 before the first edit leaves the workspace as it
// was; the build is could_not_run with the exit map's reason, never empty
// (13 "Facts, not claims"; 02).
func creditBeforeEdit(ctx context.Context, dir string) ([]Step, []string, error) {
	return creditExhausted(ctx, dir, false)
}

// creditAfterEdit: a 402 after an edit leaves a changed workspace; the build
// is a partial_patch, could_not_run, whose value is the controller's diff,
// sealed in the attempt's AttemptRecord (13; 05 "AttemptRecord"; 11:160).
func creditAfterEdit(ctx context.Context, dir string) ([]Step, []string, error) {
	return creditExhausted(ctx, dir, true)
}

// The fixture's tree: one file, its base content, and the edit.
const (
	buildFile = "inventory.py"
	buildBase = "def total(items):\n    return 0\n"
	buildEdit = "def total(items):\n    return sum(items)\n"
)

// creditExhausted runs one build attempt through the real adapter host with
// a scripted peer (package inproc) that edits the workspace or not, then
// reports a 402 (an error event credit_exhausted) and could_not_run. The
// controller computes the result from its own two facts: completion (the
// submitted result) and its diff of the workspace against the base.
func creditExhausted(ctx context.Context, dir string, edit bool) (steps []Step, violations []string, err error) {
	reg, err := vocab.Load()
	if err != nil {
		return nil, nil, err
	}
	state, root, base, work := filepath.Join(dir, "state"), filepath.Join(dir, "exchange"), filepath.Join(dir, "base"), filepath.Join(dir, "work")
	for _, d := range []string{root, base, work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, nil, err
		}
	}
	for _, d := range []string{base, work} {
		if err := os.WriteFile(filepath.Join(d, buildFile), []byte(buildBase), 0o600); err != nil {
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
	s, err := eventstore.Open(ctx, lock, eventstore.Config{Kinds: kinds, Blobs: blobs,
		Hooks: eventstore.Hooks{Identify: func(*eventstore.Tx, eventstore.Command) error { return nil }, Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil },
			Project: func(tx *eventstore.Tx, evs []eventstore.Event) error {
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

	// The build node and its key: one plan input, resolved.
	planRes, err := ids.New("res")
	if err != nil {
		return nil, nil, err
	}
	node, key, err := buildNode(types.ResultID(planRes))
	if err != nil {
		return nil, nil, err
	}

	// The attempt: a granted lease, then the host runs the scripted peer.
	l, att, err := freshLease()
	cid, ierr := ids.New("cal")
	if err = errors.Join(err, ierr); err != nil {
		return nil, nil, err
	}
	if err := do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, l) }); err != nil {
		return nil, nil, err
	}
	if err := do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, l.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, reg.Holds)
	}); err != nil {
		return nil, nil, err
	}
	var failed error
	baseTree := types.GitID("git:" + strings.Repeat("c", 40))
	host := &adapterhost.Host{Store: s, Blobs: blobs, Actor: actor, Root: root}
	out, err := startedRun(ctx, host, do, 2, adapterhost.Spec{AnyTool: true, Lease: l.ID, Attempt: att, Call: types.CallID(cid), Prompt: []byte("implement total\n"),
		Base:    types.AttemptBase{Tree: baseTree, ContractRevision: "7"},
		Adapter: session.Config{InProcess: inproc.New(creditPeer(edit, &failed)), Deadline: 30 * time.Second},
		Prepare: types.AdapterPrepareParams{Role: "builder", Executor: l.Executor, Model: "fixture-model",
			Workspace:  types.AdapterPrepareParamsWorkspace{Path: work, Writable: true},
			Limits:     types.AdapterPrepareParamsLimits{MaxTurns: 8, MaxOutputTokens: 8000, WallSeconds: 60},
			ToolPolicy: types.AdapterPrepareParamsToolPolicy{Allowed: []string{"Edit"}, Mediated: []string{}}}})
	switch {
	case err != nil:
		return nil, nil, err
	case failed != nil:
		return nil, nil, fmt.Errorf("the peer: %w", failed)
	case out.Submitted == nil:
		return nil, nil, fmt.Errorf("the attempt's result was not submitted: %v", out.Session)
	}

	// The two facts, and the result the table computes from them.
	d, err := treeDiff(base, work)
	if err != nil {
		return nil, nil, err
	}
	// No status: the host keeps no error excerpt once the run ends, so the
	// controller has no observed status here, only the hints.
	facts := graph.BuildFacts{Project: "prj_01m3250vkwffc979nzjw0g2sf9", Submitted: true, Reported: out.Submitted.Outcome,
		Exit: exitmap.Exit{Protocol: out.Result == nil, Hints: out.Hints}}
	if len(d) > 0 {
		digest, err := blobs.Put(bytes.NewReader(d), "")
		if err != nil {
			return nil, nil, err
		}
		facts.Diff = types.Digest(digest)
	}
	r, err := graph.Build(key, facts)
	if err != nil {
		return nil, nil, err
	}
	var o observe
	o.check(slices.Equal(out.Hints, []string{"credit_exhausted"}), "the run's error hints are %v", out.Hints)
	o.steps = append(o.steps, Step{vocab.Outcome(r.Outcome), r.Reason.String()})
	o.check(graph.Check(r) == nil, "the result is not a valid records/result: %v", graph.Check(r))
	o.check(r.Computation == key.Key && r.ID != types.ResultID(key.Key), "the result does not answer the node's key %s", key.Key)
	o.check(node.Kind == types.NodeKindBuild, "not a build node")

	// The attempt is sealed with its AttemptRecord: the partial diff, if
	// the result carries one, is its checkpoint.
	var partial *types.Digest
	if r.Kind == types.ResultKindPartialPatch && len(r.Value) == 1 {
		partial = &r.Value[0].ContentDigest
	}
	if err := seal(do, blobs, actor, att, baseTree, partial); err != nil {
		return nil, nil, err
	}
	rec, err := sealed(ctx, s, blobs, att)
	if err != nil {
		return nil, nil, err
	}
	if edit {
		has, err := blobs.Has(string(facts.Diff))
		if err != nil {
			return nil, nil, err
		}
		o.check(rec.PartialDiff != nil && *rec.PartialDiff == facts.Diff && has, "the sealed record's partial diff is %v, want the published %s", rec.PartialDiff, facts.Diff)
	} else {
		o.check(rec.PartialDiff == nil, "the sealed record of an unchanged tree names a partial diff %v", rec.PartialDiff)
	}
	if edit {
		o.check(r.Kind == types.ResultKindPartialPatch && partial != nil && *partial == facts.Diff,
			"the partial work is not the result: %s with value %v, diff %s", r.Kind, r.Value, facts.Diff)
	} else {
		o.check(r.Kind == types.ResultKindPatch && len(r.Value) == 0, "an unchanged tree produced %s with value %v", r.Kind, r.Value)
	}
	return o.steps, o.violations, nil
}

// buildNode is the fixture's build node, and its key at generation 1.
func buildNode(plan types.ResultID) (types.Node, types.ComputationKey, error) {
	nid, err := ids.New("nod")
	pid, perr := ids.New("nod")
	if err = errors.Join(err, perr); err != nil {
		return types.Node{}, types.ComputationKey{}, err
	}
	n := types.Node{ID: types.NodeID(nid), Kind: types.NodeKindBuild, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{"task":"implement total"}`),
		Inputs:   []types.InputSpec{{Name: "plan", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: types.NodeID(pid), ResultKind: types.ResultKindPlan}}}},
		Requires: types.NodeRequires{Role: ptr(types.Name("builder")), Capability: ptr(types.Name("edit")), Resources: []types.ResourceGroupKind{types.ResourceGroupKindAccountQuota}}}
	k, err := graph.Key(n, map[string]graph.Resolved{"plan": {Result: plan}}, 1)
	return n, k, err
}

func ptr[T any](v T) *T { return &v }

// seal publishes the attempt's AttemptRecord and records attempt.sealed:
// the projector admits only a valid record of this attempt whose every blob
// is published (05).
func seal(do func(func(*eventstore.Tx) ([]eventstore.Event, error)) error, blobs *blobstore.Store, actor string,
	att types.AttemptID, base types.GitID, partial *types.Digest) error {
	put := func(b string) (types.Digest, error) {
		d, err := blobs.Put(strings.NewReader(b), "")
		return types.Digest(d), err
	}
	trail, err := put("the fixture's trail\n")
	manifest, merr := put("the fixture's evidence manifest\n")
	if err = errors.Join(err, merr); err != nil {
		return err
	}
	rec := types.AttemptRecord{Attempt: att, Project: "prj_01m3250vkwffc979nzjw0g2sf9", Task: "tsk_01m3250v9xft08ssr4ygq6g2np", ContractRevision: "7",
		FencingToken: 1, BaseTree: base, PartialDiff: partial, FailingTests: []string{}, Trail: trail, EvidenceManifest: manifest, Pinned: false, // M1 L1: nothing continues from it
		Seal: types.AttemptRecordSeal{Reason: types.AttemptRecordSealReasonCompleted, At: time.Now().UTC().Format(time.RFC3339), SealedBy: types.ActorID(actor)}}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	checkpoint, err := put(string(b))
	if err != nil {
		return err
	}
	return do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeAttempt(tx, att, 2, lease.AttemptEvent{Kind: lease.Sealed, Checkpoint: checkpoint})
	})
}

// excerpt is the raw excerpt of the peer's error event.
const excerpt = "HTTP 402: insufficient credits\n"

// sealed reads the attempt back and decodes the AttemptRecord its seal names.
func sealed(ctx context.Context, s *eventstore.Store, blobs *blobstore.Store, att types.AttemptID) (types.AttemptRecord, error) {
	var raw json.RawMessage
	if err := s.View(ctx, func(tx *eventstore.Tx) (err error) {
		raw, _, err = tx.Get(lease.AttemptProjection, string(att))
		return err
	}); err != nil {
		return types.AttemptRecord{}, err
	}
	var a struct {
		Record types.Attempt `json:"record"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return types.AttemptRecord{}, err
	}
	if a.Record.Status != types.AttemptStatusSealed || a.Record.Checkpoint == nil {
		return types.AttemptRecord{}, fmt.Errorf("the attempt is %s with checkpoint %v, not sealed", a.Record.Status, a.Record.Checkpoint)
	}
	rc, err := blobs.Open(string(*a.Record.Checkpoint))
	if err != nil {
		return types.AttemptRecord{}, err
	}
	b, err := io.ReadAll(rc)
	if err = errors.Join(err, rc.Close()); err != nil {
		return types.AttemptRecord{}, err
	}
	return contract.Decode[types.AttemptRecord]("records/attempt-record", b)
}

// treeDiff is the fake sandbox's diff of work against base: one line per
// path whose bytes differ or that only one side has, sorted; empty when the
// trees are the same.
func treeDiff(base, work string) ([]byte, error) {
	files := map[string][2][]byte{}
	for i, root := range []string{base, work} {
		err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			f := files[rel]
			f[i] = b
			files[rel] = f
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	var out bytes.Buffer
	for _, p := range slices.Sorted(maps.Keys(files)) {
		if f := files[p]; !bytes.Equal(f[0], f[1]) {
			fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n%s", p, p, f[1])
		}
	}
	return out.Bytes(), nil
}

// creditPeer edits the workspace (or not), reports a 402 as an error event
// and returns could_not_run, as adapters/fake's credit-exhausted scenario
// does.
func creditPeer(edit bool, failed *error) func(io.Reader, io.Writer) {
	return peer(func(p types.AdapterPrepareParams, params json.RawMessage, send func(any) error) (json.RawMessage, error) {
		return creditRun(edit, p.Workspace.Path, p.Exchange.Out, params, send)
	}, failed)
}

func creditRun(edit bool, workspace, outDir string, params json.RawMessage, send func(any) error) (json.RawMessage, error) {
	var p types.AdapterRunParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if edit {
		if err := os.WriteFile(filepath.Join(workspace, buildFile), []byte(buildEdit), 0o600); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(outDir, "error-1.txt"), []byte(excerpt), 0o644); err != nil {
		return nil, err
	}
	h := declared(p.Attempt, excerpt)
	h.Handle = "error-1.txt"
	ev := types.ErrorEvent{Event: "error", Attempt: p.Attempt, FencingToken: p.FencingToken, Call: p.Call, Turn: 1,
		ClassHint: types.ErrorEventClassHintCreditExhausted, RawExcerpt: h}
	for range 2 { // the provider refuses twice: the hints are a set
		if err := send(map[string]any{"jsonrpc": wire.Version, "method": wire.EventMethod, "params": ev}); err != nil {
			return nil, err
		}
	}
	// The adapter's own reason is not the credit one: the result's reason
	// must come from the exit map.
	return json.Marshal(types.AdapterRunResult{Outcome: types.OutcomeCouldNotRun, Reason: vocab.ReasonProviderUnavailable,
		Detail: "the fake provider: credit_exhausted"})
}
