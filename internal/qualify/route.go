// SPDX-License-Identifier: Apache-2.0

//go:build linux

package qualify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/review"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab/exitmap"
)

// Route is one registry reviewer executor, run through the real adapter
// host, http-chat adapter and gateway, each review on a throwaway state of
// its own: a granted reviewer lease, the gateway bound to its one call and
// served on a Unix socket (ADC_GATEWAY_SOCKET), as P22a will serve it.
type Route struct {
	Registry registry.Registry
	Executor types.ExecutorID // a reviewer of Registry
	Secrets  gateway.Secrets  // gateway.EnvSecrets{} on a live run
	Adapter  session.Config   // the http-chat adapter's command and environment
	Bounds   adapterhost.Bounds
}

// Review runs one review of case c's prompt in run: its decision
// (review.Go, review.NoGo, or "" when the reviewer returned none, or one
// review.Parse refuses), its D55 report (review.Report, through
// review.Result) and the evidence of the execution (Astra #306 H1): run's
// id, start, adapter and suite, the case's place in the run's order (from
// the run's cursor), the digest of the prompt the adapter host was given,
// the route of the registry executor the review ran on, and the review's
// model call. It refuses a case that is not the run's next in its seed's
// order, a prompt that is not the case's (Opus #306 r2 M1), and an
// adapter command other than the http-chat adapter under the suite's root,
// which AdapterDigest binds (r2 L3).
func (r Route) Review(ctx context.Context, run *Run, c Case, prompt []byte) (o Outcome, err error) {
	e, ok := r.Registry.Executor(r.Executor)
	if !ok || e.Role != types.RegistryExecutorRoleReviewer {
		return o, fmt.Errorf("qualify: %s is not a reviewer of the registry", r.Executor)
	}
	if run == nil || run.st == nil {
		return o, errors.New("qualify: not a run Begin started")
	}
	if cmd := r.Adapter.Command; len(cmd) != 2 || cmd[1] != filepath.Join(run.st.root, AdapterEntry) {
		return o, fmt.Errorf("qualify: the adapter command %q is not %s under %s, which AdapterDigest binds", cmd, AdapterEntry, run.st.root)
	}
	if digest(prompt) != c.PromptDigest {
		return o, fmt.Errorf("qualify: case %s: the prompt is not the case's", c.ID)
	}
	seq, err := run.take(c)
	if err != nil {
		return o, err
	}
	dir, err := os.MkdirTemp("", "adc-qualify-")
	if err != nil {
		return o, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	state, root, work := filepath.Join(dir, "state"), filepath.Join(dir, "exchange"), filepath.Join(dir, "workspace")
	for _, d := range []string{root, work} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return o, err
		}
	}
	lock, err := eventstore.AcquireLock(state)
	if err != nil {
		return o, err
	}
	defer func() { err = errors.Join(err, lock.Release()) }()
	blobs, err := blobstore.Open(state)
	if err != nil {
		return o, err
	}
	reg, err := vocab.Load()
	if err != nil {
		return o, err
	}
	idv := map[string]string{}
	for _, p := range []string{"act", "lea", "att", "cal", "unt", "prj"} {
		if idv[p], err = ids.New(p); err != nil {
			return o, err
		}
	}
	kinds := lease.Kinds()
	maps.Copy(kinds, modelcall.Kinds())
	maps.Copy(kinds, ledger.Kinds())
	allow := func(*eventstore.Tx, eventstore.Command) error { return nil } // the run's own store: no other actor writes it
	s, err := eventstore.Open(ctx, lock, eventstore.Config{Kinds: kinds, Blobs: blobs, Effects: modelcall.Effects(), Hooks: eventstore.Hooks{Identify: allow, Authorize: allow,
		Project: eventstore.ProjectEach(lease.Projector{Blobs: blobs, Holds: reg.Holds, Units: soleUnit(idv["unt"])}.Project, modelcall.Project, ledger.Project)}})
	if err != nil {
		return o, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	if _, err := s.MarkPendingUncertain(ctx, idv["act"]); err != nil { // adcd's start rule, which the model-call recorder requires
		return o, err
	}
	ok2 := types.PlacementCheck{Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean}
	l := types.Lease{ID: types.LeaseID(idv["lea"]), Unit: types.UnitID(idv["unt"]), Role: "reviewer", Executor: e.ID, State: types.LeaseStateRequested,
		Reservations: []types.LeaseReservationsItem{{Group: e.Group, Amount: 200_000, Unit: types.CapacityUnitTokens}},
		Footprint:    types.LeaseFootprint{ContextTokens: 100_000, ExpectedTurns: 1}, Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		PlacementChecks: types.LeasePlacementChecks{DataFloor: ok2, Destinations: ok2, Capability: ok2, Independence: ok2, PaidAuthority: ok2, Budget: ok2, FallbackScope: ok2}}
	submit := func(step func(*eventstore.Tx) ([]eventstore.Event, error)) error {
		cmd, err := ids.New("cmd")
		if err != nil {
			return err
		}
		_, err = s.SubmitApplied(ctx, eventstore.Command{ID: cmd, Actor: idv["act"], Action: "qualify.lease", IdempotencyKey: cmd, Input: []byte(`{}`)},
			eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				evs, err := step(tx)
				return evs, json.RawMessage(`{}`), err
			}))
		return err
	}
	for _, step := range []func(*eventstore.Tx) ([]eventstore.Event, error){
		func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, l) },
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.ChangeLease(tx, l.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, reg.Holds)
		},
	} {
		if err := submit(step); err != nil {
			return o, err
		}
	}
	rec := modelcall.Recorder{Store: s, Actor: idv["act"]}
	g, err := gateway.New(gateway.Config{Registry: r.Registry, Leases: modelcall.Reader{Store: s}, Attempts: modelcall.Reader{Store: s}, Secrets: r.Secrets, Intents: rec, Counts: rec})
	if err != nil {
		return o, err
	}
	call := types.CallID(idv["cal"])
	token, err := g.Bind(gateway.Binding{Lease: l.ID, Attempt: types.AttemptID(idv["att"]), Call: call})
	if err != nil {
		return o, err
	}
	sock := filepath.Join(dir, "gateway.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return o, err
	}
	srv := &http.Server{Handler: g, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { err = errors.Join(err, srv.Close()) }()
	adapter := r.Adapter
	adapter.Env = append(append([]string{}, adapter.Env...), "ADC_GATEWAY_SOCKET="+sock)
	limits := types.AdapterPrepareParamsLimits{MaxTurns: MaxTurns, MaxOutputTokens: MaxOutputTokens, WallSeconds: max(1, int64(cmpDur(r.Bounds.Result, 10*time.Minute)/time.Second))}
	// Tools: adcd's launch allowance, the edit loop's own tools, which the
	// http-chat adapter always describes; a reviewer is prepared with none
	// (ToolPolicy.Allowed is empty, and the adapter sends no tools).
	spec := adapterhost.Spec{Tools: adapterhost.EditLoopTools(), Lease: l.ID, Attempt: types.AttemptID(idv["att"]), Call: call, Prompt: prompt,
		Bounds: r.Bounds, Adapter: adapter,
		Base:    types.AttemptBase{Tree: types.GitID("git:" + strings.Repeat("0", 40)), ContractRevision: "1"},
		Prepare: reviewerPrepare(e.ID, e.Model, work, token, limits)}
	start := time.Now()
	// The host never starts an attempt (P22a-5c-w): the exchange is
	// prepared and checked, then the run, its own controller, starts the
	// attempt on its granted lease, then the host runs it.
	p, err := (&adapterhost.Host{Store: s, Blobs: blobs, Actor: idv["act"], Root: root}).Prepare(spec)
	if err != nil {
		return o, err
	}
	if err := submit(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.StartAttempt(tx, l.ID, 2, spec.Attempt, spec.Base)
	}); err != nil {
		return o, errors.Join(err, p.Discard())
	}
	out, err := p.Run(ctx)
	if err != nil {
		return o, err
	}
	// The charge link (chargeLinks).
	var calls []types.CallID
	if err := s.View(ctx, func(tx *eventstore.Tx) error {
		ms, err := ledger.Measurements(tx, call)
		calls = chargeLinks(call, ms)
		return err
	}); err != nil {
		return o, err
	}
	// GateIDs: none. review.Prompt renders the gate's summary (its verdict
	// and counts), never a gate report's ids, so the findings view a no_go
	// gets (#361) has none to redact.
	f := review.Facts{Exit: exitmap.Exit{Protocol: out.Session != nil, Hints: out.Hints}, CandidateRound: 1, Calls: calls,
		Roster: []types.RosterMember{{Executor: e.ID, Family: e.Family}}, Elapsed: time.Since(start), Project: types.ProjectID(idv["prj"]), GateIDs: []string{}}
	if out.Submitted != nil && out.Submitted.Blob != nil {
		f.Submitted, f.Decision = true, *out.Submitted.Blob
	}
	res, err := review.Result(types.ComputationKey{Key: digest(prompt), Generation: 1}, f, blobs)
	if err != nil {
		return o, err
	}
	rc, err := blobs.Open(string(res.Value[len(res.Value)-1].ContentDigest))
	if err != nil {
		return o, err
	}
	o.Report, err = io.ReadAll(rc)
	if err = errors.Join(err, rc.Close()); err != nil {
		return o, err
	}
	switch res.Outcome {
	case types.OutcomeEmpty:
		o.Decision = review.Go
	case types.OutcomeFound:
		o.Decision = review.NoGo
	}
	o.evidence = &types.QualifiedEvidence{RunID: run.ID, RanAt: types.Time(run.At.Format(time.RFC3339)), Seq: int64(seq), PromptDigest: digest(spec.Prompt),
		Route: RouteOf(e), AdapterDigest: run.Adapter, SuiteDigest: run.Suite, Call: call}
	run.log(c.ID, *o.evidence)
	return o, nil
}

// reviewerPrepare is a reviewer's prepare params on executor's model: a
// read-only workspace at work, the gateway's token, limits, and no tool,
// neither its own nor mediated (P31a: a reviewer runs no tool; the
// adapter's own refusal to offer one is not this check).
func reviewerPrepare(executor types.ExecutorID, model types.AdapterLabel, work, token string, limits types.AdapterPrepareParamsLimits) types.AdapterPrepareParams {
	return types.AdapterPrepareParams{Role: "reviewer", Executor: executor, Model: model, Limits: limits,
		Workspace:  types.AdapterPrepareParamsWorkspace{Path: work, Writable: false},
		Gateway:    &types.AdapterPrepareParamsGatewayValue{BaseUrl: "http://gateway", Token: token},
		ToolPolicy: types.AdapterPrepareParamsToolPolicy{Allowed: []string{}, Mediated: []string{}}}
}

// soleUnit is the run's unit binding (lease.Units), as adc-harness's: its
// throwaway store holds no graph.
type soleUnit string

func (soleUnit) ProjectTask(*eventstore.Tx, types.UnitID) (types.ProjectID, types.TaskID, bool, error) {
	return "", "", false, nil
}

func (u soleUnit) Startable(_ *eventstore.Tx, unit types.UnitID) error {
	if unit != types.UnitID(u) {
		return fmt.Errorf("qualify: unit %s is not the run's", unit)
	}
	return nil
}

func cmpDur(d, otherwise time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return otherwise
}

// chargeLinks is a review's charge links: its call, only when the gateway
// counted a request of it in ms (a gateway_floor or gateway_count
// measurement; an adapter's own usage report is no request, Opus #417 L1).
// An adapter that ended before any request (one that never started) made
// no call to link.
func chargeLinks(call types.CallID, ms []types.UsageMeasurement) []types.CallID {
	if slices.ContainsFunc(ms, func(m types.UsageMeasurement) bool {
		return m.SourceOfTruth == types.UsageMeasurementSourceOfTruthGatewayFloor || m.SourceOfTruth == types.UsageMeasurementSourceOfTruthGatewayCount
	}) {
		return []types.CallID{call}
	}
	return nil
}
