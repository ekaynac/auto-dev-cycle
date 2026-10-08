// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Package harness is `adc-harness run` (docs/design/m0/15 S1): one attempt
// through internal/adapterhost against an adapter spawned in its PID
// namespace, on a throwaway state of its own, removed when the run ends.
// It does not drive the installed daemon: that comes with M1's wired lease
// and attempt actions (operator 2026-10-08, option C).
//
// Every wait on the adapter is bounded (adapterhost.Bounds), and a bound
// that passes ends the run could_not_run, exit 2, its reason by the exit
// map with the stop requested (attempt_stopped unless a capacity hint came
// first). The report holds no text of the adapter's beyond the handle names
// it chose, which can reach the output (escaped): never its stderr, its
// result's detail, its environment or the prompt. It is written through the
// CLI's escaping (cli.Say, cli.SayJSON).
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/cli"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab/exitmap"
)

// The waits' default bounds, and the slack the session's deadline adds to
// their sum.
const (
	DefaultStart     = 30 * time.Second
	DefaultHandshake = 30 * time.Second
	DefaultResult    = 10 * time.Minute
	DefaultShutdown  = 5 * time.Second
	deadlineSlack    = 5 * time.Second
)

// MaxPrompt bounds --prompt's file; defaultPrompt is the prompt without one.
const (
	MaxPrompt     = 1 << 20
	defaultPrompt = "Run the dry-run task.\n"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Report is what a run prints: its outcome, the attempt's submission and
// usage records, and its output refusals.
type Report struct {
	Outcome   types.Outcome                `json:"outcome"`
	Reason    string                       `json:"reason"`
	Detail    string                       `json:"detail"`
	Attempt   types.AttemptID              `json:"attempt"`
	Submitted *types.SubmittedResult       `json:"submitted"`
	Usage     []types.UsageMeasurement     `json:"usage"`
	Refusals  []types.AttemptOutputRefusal `json:"refusals"`
}

type envFlag []string

func (e *envFlag) String() string     { return strings.Join(*e, ",") }
func (e *envFlag) Set(v string) error { *e = append(*e, v); return nil }

// Main runs `adc-harness run [flags] -- COMMAND...` and returns its exit
// code (08's table). getenv is the harness's environment: the adapter gets
// only its PATH, a TMPDIR of the run's own, and each --env NAME.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("adc-harness run", flag.ContinueOnError)
	var b adapterhost.Bounds
	fs.DurationVar(&b.Start, "start-timeout", DefaultStart, "bound from the spawn to describe's reply")
	fs.DurationVar(&b.Handshake, "handshake-timeout", DefaultHandshake, "bound on prepare's reply")
	fs.DurationVar(&b.Result, "result-timeout", DefaultResult, "bound on run's result")
	fs.DurationVar(&b.Shutdown, "shutdown-timeout", DefaultShutdown, "bound on shutdown's reply")
	promptFile := fs.String("prompt", "", "the prompt's file (at most 1 MiB; default a fixed dry-run prompt)")
	workspace := fs.String("workspace", "/workspace", "the workspace path prepare names (absolute)")
	asJSON := fs.Bool("json", false, "print the report as one JSON document")
	var env envFlag
	fs.Var(&env, "env", "pass this variable of the harness's environment to the adapter (repeatable)")
	if code, ok := cli.Parse(fs, args, stderr); !ok {
		if code == entry.ExitOK {
			_ = cli.Say(stderr, "usage: adc-harness run [flags] -- COMMAND [ARG...]\n")
		}
		return code
	}
	usage := func(format string, a ...any) int {
		_ = cli.Say(stderr, "adc-harness run: "+format+"\n", a...)
		return entry.ExitUsage
	}
	command := fs.Args()
	if len(command) == 0 {
		return usage("no adapter command; usage: adc-harness run [flags] -- COMMAND [ARG...]")
	}
	for i, d := range []time.Duration{b.Start, b.Handshake, b.Result, b.Shutdown} {
		if d <= 0 {
			return usage("--%s-timeout must be positive", []string{"start", "handshake", "result", "shutdown"}[i])
		}
	}
	if !filepath.IsAbs(*workspace) {
		return usage("--workspace must be an absolute path")
	}
	adapterEnv := []string{"PATH=" + getenv("PATH")}
	for _, name := range env {
		if !envName.MatchString(name) || name == "PATH" || name == "TMPDIR" {
			return usage("--env %s is not a variable name the harness passes", name)
		}
		v := getenv(name)
		if v == "" {
			return usage("--env %s is not set", name)
		}
		adapterEnv = append(adapterEnv, name+"="+v)
	}
	prompt, err := readPrompt(*promptFile)
	if err != nil {
		return usage("%s", err)
	}
	r, out, err := run(ctx, command, adapterEnv, *workspace, prompt, b)
	if err != nil { // the host's texts are fixed; the harness's name only its temporary state
		_ = cli.Say(stderr, "adc-harness run: %s\n", err)
	}
	rep, code := report(ctx, r, out, err)
	if werr := write(stdout, rep, *asJSON); werr != nil {
		_ = cli.Say(stderr, "adc-harness run: could_not_run: writing the report failed\n")
		return entry.ExitCouldNotRun
	}
	return code
}

// readPrompt reads --prompt, refusing a file over MaxPrompt.
func readPrompt(name string) ([]byte, error) {
	if name == "" {
		return []byte(defaultPrompt), nil
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, errors.New("--prompt cannot be read")
	}
	defer f.Close()
	p, err := io.ReadAll(io.LimitReader(f, MaxPrompt+1))
	switch {
	case err != nil:
		return nil, errors.New("--prompt cannot be read")
	case len(p) > MaxPrompt:
		return nil, fmt.Errorf("--prompt is over %d bytes", MaxPrompt)
	}
	return p, nil
}

// run grants a fresh lease on a throwaway store and runs one attempt.
func run(ctx context.Context, command, env []string, workspace string, prompt []byte, b adapterhost.Bounds) (spec adapterhost.Spec, out adapterhost.Outcome, err error) {
	dir, err := os.MkdirTemp("", "adc-harness-")
	if err != nil {
		return spec, out, err
	}
	// A tree not seen to exit may still be writing: then no chmod walk.
	defer func() { err = errors.Join(err, removeState(dir, !errors.Is(out.Session, session.ErrUnverified))) }()
	state, root, tmp := filepath.Join(dir, "state"), filepath.Join(dir, "exchange"), filepath.Join(dir, "tmp")
	for _, d := range []string{root, tmp} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return spec, out, err
		}
	}
	lock, err := eventstore.AcquireLock(state)
	if err != nil {
		return spec, out, err
	}
	defer func() { err = errors.Join(err, lock.Release()) }()
	blobs, err := blobstore.Open(state)
	if err != nil {
		return spec, out, err
	}
	reg, err := vocab.Load()
	if err != nil {
		return spec, out, err
	}
	kinds := lease.Kinds()
	maps.Copy(kinds, ledger.Kinds())
	project := lease.Projector{Blobs: blobs, Holds: reg.Holds}.Project
	allow := func(*eventstore.Tx, eventstore.Command) error { return nil } // the run's own store: no other actor writes it
	s, err := eventstore.Open(ctx, lock, eventstore.Config{Kinds: kinds, Blobs: blobs, Hooks: eventstore.Hooks{Identify: allow, Authorize: allow,
		Project: func(tx *eventstore.Tx, evs []eventstore.Event) error {
			return errors.Join(project(tx, evs), ledger.Project(tx, evs))
		}}})
	if err != nil {
		return spec, out, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	idv := map[string]string{}
	for _, p := range []string{"act", "lea", "unt", "exe", "att", "cal"} {
		if idv[p], err = ids.New(p); err != nil {
			return spec, out, err
		}
	}
	ok := types.PlacementCheck{Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean}
	l := types.Lease{ID: types.LeaseID(idv["lea"]), Unit: types.UnitID(idv["unt"]), Role: "builder", Executor: types.ExecutorID(idv["exe"]),
		State:        types.LeaseStateRequested,
		Reservations: []types.LeaseReservationsItem{{Group: "rg_01m3250vkwffc979nzjw0g2sf9", Amount: 1000, Unit: types.CapacityUnitTokens}},
		Footprint:    types.LeaseFootprint{ContextTokens: 1000, ExpectedTurns: 5}, Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		PlacementChecks: types.LeasePlacementChecks{DataFloor: ok, Destinations: ok, Capability: ok, Independence: ok,
			PaidAuthority: ok, Budget: ok, FallbackScope: ok}}
	for _, step := range []func(*eventstore.Tx) ([]eventstore.Event, error){
		func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, l) },
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.ChangeLease(tx, l.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, reg.Holds)
		},
	} {
		cmd, err := ids.New("cmd")
		if err != nil {
			return spec, out, err
		}
		if _, err := s.SubmitApplied(ctx, eventstore.Command{ID: cmd, Actor: idv["act"], Action: "harness.lease", IdempotencyKey: cmd, Input: []byte(`{}`)},
			eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				evs, err := step(tx)
				return evs, json.RawMessage(`{}`), err
			})); err != nil {
			return spec, out, err
		}
	}
	spec = adapterhost.Spec{Lease: l.ID, LeaseRevision: 2, Attempt: types.AttemptID(idv["att"]), Call: types.CallID(idv["cal"]), Prompt: prompt, Bounds: b,
		Base: types.AttemptBase{Tree: types.GitID("git:" + strings.Repeat("0", 40)), ContractRevision: "1"},
		Adapter: session.Config{Command: command, Env: append(env, "TMPDIR="+tmp),
			Deadline: sessionDeadline(b)},
		Prepare: types.AdapterPrepareParams{Role: "builder", Executor: l.Executor, Model: "harness-model",
			Workspace:  types.AdapterPrepareParamsWorkspace{Path: workspace, Writable: true},
			Limits:     types.AdapterPrepareParamsLimits{MaxTurns: 8, MaxOutputTokens: 8000, WallSeconds: max(1, int64(b.Result/time.Second))},
			ToolPolicy: types.AdapterPrepareParamsToolPolicy{Allowed: []string{"Edit"}, Mediated: []string{}}}}
	host := &adapterhost.Host{Store: s, Blobs: blobs, Actor: idv["act"], Root: root}
	out, err = host.Run(ctx, spec)
	return spec, out, err
}

// sessionDeadline is the session's deadline, the backstop behind the
// waits' bounds: their sum and some slack.
func sessionDeadline(b adapterhost.Bounds) time.Duration {
	return b.Start + b.Handshake + b.Result + b.Shutdown + deadlineSlack
}

// removeState removes the run's temporary state. The adapter shares the
// file system, so, when walk is set (its process tree was seen to exit), it
// first gives the owner rwx on every directory in it (walked without
// following symlinks); what still cannot be removed is reported by its path.
func removeState(dir string, walk bool) error {
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if walk && err == nil && d.IsDir() {
			if info, ierr := d.Info(); ierr == nil {
				_ = os.Chmod(p, info.Mode().Perm()|0o700) // RemoveAll reports what this leaves
			}
		}
		return nil
	})
	if os.RemoveAll(dir) != nil {
		return fmt.Errorf("the temporary state %s could not be removed", dir)
	}
	return nil
}

// report maps a run to its report and exit code (08): a passed bound or the
// caller's stop could_not_run by the exit map with the stop requested; a
// submission by its outcome, whatever error followed it; a refusal failed
// (stale_attempt: exit 4); anything else could_not_run by the exit map.
// Every detail is fixed text.
func report(ctx context.Context, spec adapterhost.Spec, out adapterhost.Outcome, err error) (Report, int) {
	r := Report{Attempt: spec.Attempt, Submitted: out.Submitted, Usage: append([]types.UsageMeasurement{}, out.Usage...),
		Refusals: append([]types.AttemptOutputRefusal{}, out.Refusals...)}
	cnr := func(reason vocab.Reason, detail string) (Report, int) {
		r.Outcome, r.Reason, r.Detail, r.Submitted = types.OutcomeCouldNotRun, reason.String(), detail, nil
		return r, entry.ExitCouldNotRun
	}
	stopped := exitmap.Reason(exitmap.Exit{StopRequested: true, Hints: out.Hints})
	var te *adapterhost.TimeoutError
	switch {
	case errors.As(out.Session, &te):
		return cnr(stopped, "the adapter's "+te.Wait+" passed its bound")
	case out.Submitted != nil:
		r.Outcome, r.Reason, r.Detail = out.Submitted.Outcome, out.Submitted.Reason.String(), "the attempt's result was submitted"
		switch r.Outcome {
		case types.OutcomeFailed:
			return r, entry.ExitRefused
		case types.OutcomeCouldNotRun:
			return r, entry.ExitCouldNotRun
		}
		return r, entry.ExitOK
	case ctx.Err() != nil:
		return cnr(stopped, "the harness was stopped")
	case err != nil:
		return cnr(vocab.ReasonExecutorIncomplete, "the harness could not run or finalise the attempt")
	case out.StartRefused != nil:
		return cnr(vocab.ReasonPreconditionFailed, "the host's prelaunch capability check failed")
	case len(out.Refusals) > 0:
		r.Outcome, r.Reason, r.Detail = types.OutcomeFailed, string(out.Refusals[0].Reason), "an output of the attempt was refused"
		if out.Refusals[0].Reason == types.AttemptOutputRefusalReasonStaleAttempt {
			return r, entry.ExitStale
		}
		return r, entry.ExitRefused
	}
	var se *session.Error // the core's own stop (the deadline backstop, a close) is a stop, not a protocol break
	coreStop := errors.As(out.Session, &se) && se.Reason() == vocab.ReasonAttemptStopped
	return cnr(exitmap.Reason(exitmap.Exit{StopRequested: coreStop, Protocol: out.Session != nil && !coreStop, Hints: out.Hints}),
		"the attempt ended without a submission")
}

// write prints the report: one JSON document, or its human lines.
func write(w io.Writer, r Report, asJSON bool) error {
	if asJSON {
		doc, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return cli.SayJSON(w, doc)
	}
	err := cli.Say(w, "%s/%s: %s\nattempt: %s\n", r.Outcome, r.Reason, r.Detail, r.Attempt)
	if s := r.Submitted; s != nil {
		kind, blob := "none", "none"
		if s.Kind != nil {
			kind = string(*s.Kind)
		}
		if s.Blob != nil {
			blob = string(*s.Blob)
		}
		err = errors.Join(err, cli.Say(w, "submitted: %s/%s, %s %s\n", s.Outcome, s.Reason, kind, blob))
	}
	err = errors.Join(err, cli.Say(w, "usage: %s record(s)\n", len(r.Usage)))
	for _, f := range r.Refusals {
		err = errors.Join(err, cli.Say(w, "refused: %s (%s): %s\n", f.Handle.Handle, f.Check, f.Detail))
	}
	return err
}
