// SPDX-License-Identifier: Apache-2.0

package daemon

// The gate run (P22a-5c-2b-3; W15, I13): the controller runs a claimed gate
// node itself, no model, no lease. The loop decides Gate for it
// (reconcile.Gate); the runner starts one goroutine per node, never two:
//  1. what the node takes, read in one snapshot: its claim's one input,
//     accept's result (an accepted tree, recorded with no attempt), and,
//     through accept's node, what accept took (acceptance): the build's
//     patch at its attempt's base tree;
//  2. a fresh clone of the project's repository, and the accepted tree in
//     it: accept.Accept again over the base and the patch, without its
//     guard, whose tree must be the accepted one (the tree is content
//     addressed: another is the log's inconsistency, refused);
//  3. the baseline, measured on the base tree (gate.Measure), and the gate
//     run on the accepted tree against it (gate.Run), each a bounded
//     sandbox run in the profile's one pinned image, in the profile's one
//     gate environment, under gateLimits; the changed files are the base's
//     and the tree's diff;
//  4. its result (gate.Result: the receipt, the report and the restricted
//     results), recorded with no attempt (graph.Commit) by the command the
//     receipt names.
//
// The whole run is bounded by gateBound. A failure of the run's checkout
// (the clone, the one pinned image: the configuration's), of a run the
// sandbox refused or cut, the bound, or a base that is not green is
// recorded as could_not_run/controller_run_failed with no attempt, never a
// green, and the node is held in the same command, as accept's
// (notRunOrHold). A gate run's own verdict of could_not_run/runner_fault
// (results the controller cannot read) is the gate's result, as gate.Run
// decides it. A gate the profile does not let gate run (gate.ErrSpec:
// another result adapter, environment or platform) holds the node on the
// configuration; a failure before the run records nothing and holds it.
// A base that is not green is held at once ("runner: failed"): its gate
// does not change. The gate's own could_not_run (results it cannot read)
// is held in its result's command, a bounded retry. A shutdown during a
// gate run records nothing and holds nothing (the daemon's context ended):
// the next start runs the gate again (stated, Opus 22a-5c-2b-3 L2). Inert
// until launch is active: the loop decides no Gate until then.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/accept"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gate"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/results"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// gateLimits bound each of a gate's two runs (the baseline's and the
// candidate's); gateBound bounds the whole gate run: both, the clone and
// the tree's rebuild.
var (
	gateLimits = sandbox.Limits{Memory: 4 << 30, CPUs: "2", PIDs: 1024, Timeout: 30 * time.Minute, Disk: 4 << 30}
	gateBound  = 2*gateLimits.Timeout + 10*time.Minute
)

// Gate starts node's gate run, unless one is going: true when it started
// one.
func (r *runner) Gate(node types.NodeID) bool {
	return r.start(node, func() error { return r.gateNode(node) })
}

// gating is what a gate run takes.
type gating struct {
	view   graph.NodeView
	tree   string // the accepted tree, hex
	accept acceptance
}

// gateNode is one gate run of node (the file's comment).
func (r *runner) gateNode(node types.NodeID) (err error) {
	ctx, cancel := context.WithTimeout(r.ctx, gateBound)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			perr := fmt.Errorf("the gate run panicked: %v", p)
			err = errors.Join(err, perr, guarded(func() error { return r.hold(node, perr) }))
		}
	}()
	notRun := func(why string, cause error) error {
		if ctx.Err() != nil {
			why = fmt.Sprintf("the gate did not end within its bound (%s)", gateBound)
		}
		return errors.Join(cause, r.notRunOrHold(node, types.ResultKindGateResult, "the gate could not run: "+why, cause))
	}
	g, err := r.gating(ctx, node)
	if errors.Is(err, errNotSealed) { // a backstop, as accept's: no hold, no nudge
		return err
	}
	if err != nil {
		return errors.Join(err, r.hold(node, err))
	}
	prof := g.accept.profile
	if len(prof.Gate.Environments) != 1 {
		err := fmt.Errorf("%w: the gate has %d environments, not one", errConfig, len(prof.Gate.Environments))
		return errors.Join(err, r.hold(node, err))
	}
	dir, err := os.MkdirTemp(r.clones, "gate-")
	if err != nil {
		return errors.Join(err, r.hold(node, err))
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	repo, img, err := r.checkout(ctx, prof, dir)
	if err != nil {
		return notRun(err.Error(), err)
	}
	policy, err := accept.PolicyOf(prof)
	if err != nil {
		err = fmt.Errorf("%w: %w", errConfig, err)
		return errors.Join(err, r.hold(node, err))
	}
	rebuilt, err := accept.Accept(ctx, accept.In{Repo: repo, Base: g.accept.base, Patch: g.accept.patch, Policy: policy})
	switch {
	case err != nil:
		return notRun(err.Error(), err)
	case rebuilt.Tree != g.tree:
		err := fmt.Errorf("%w: the accepted tree %s does not rebuild (%s)", errContent, g.tree, rebuilt.Tree)
		return errors.Join(err, r.hold(node, err))
	}
	changed, err := changedFiles(ctx, repo, dir, g.accept.base, g.tree)
	if err != nil {
		return notRun(err.Error(), err)
	}
	in := gate.In{Host: r.box, Image: img, Repo: repo, Tree: g.accept.base, Base: g.accept.base, Changed: changed, Gate: prof.Gate,
		Env: string(prof.Gate.Environments[0].Name), Limits: gateLimits}
	base, measured, err := gate.Measure(ctx, in)
	switch {
	case errors.Is(err, gate.ErrSpec):
		err = fmt.Errorf("%w: %w", errConfig, err)
		return errors.Join(err, r.hold(node, err))
	case err != nil:
		return notRun(err.Error(), err)
	case measured.Outcome != vocab.Empty && redBase(measured.Results):
		// A red base: it does not change, so no retry helps; it is held at
		// once ("runner: failed"), the measure's why its detail (Opus
		// 22a-5c-2b-3 M2).
		cause := fmt.Errorf("%w: the base tree's gate is not green: %s", errContent, measured.Why)
		return notRun(cause.Error(), cause)
	case measured.Outcome != vocab.Empty: // no results, or a run that did not end on its own: the sandbox's, a bounded retry
		cause := fmt.Errorf("the base tree's gate did not measure: %s", measured.Why)
		return notRun(cause.Error(), cause)
	}
	in.Tree, in.Baseline = g.tree, &base
	v, err := gate.Run(ctx, in)
	switch {
	case errors.Is(err, gate.ErrSpec):
		err = fmt.Errorf("%w: %w", errConfig, err)
		return errors.Join(err, r.hold(node, err))
	case err != nil:
		return notRun(err.Error(), err)
	}
	contract, err := gate.Contract(prof.Gate, img)
	if err != nil {
		return errors.Join(err, r.hold(node, err))
	}
	cmd, err := ids.New("cmd")
	if err != nil {
		return errors.Join(err, r.hold(node, err))
	}
	commit := func(tx *eventstore.Tx) error {
		cur, err := graph.ViewOf(tx, node)
		if err != nil || cur.State.State != types.NodeStateStateClaimed || cur.State.Key == nil {
			return errors.Join(err, fmt.Errorf("gate node %s holds no claim", node))
		}
		res, err := gate.Result(*cur.State.Key, v, contract, img, gate.Subject{Project: g.view.Project, Revision: buildContractRevision},
			types.CommandID(cmd), r.blobs, tx.Now())
		if err != nil {
			return err
		}
		return graph.Commit(tx, node, res, r.env)
	}
	if v.Outcome == vocab.CouldNotRun {
		// The gate's own could_not_run (results it cannot read: a candidate
		// may cause it) is never selected: the node is held in the same
		// command, a bounded retry ending in "runner: failed", so it never
		// re-runs at once (Opus 22a-5c-2b-3 H1).
		err = r.holdAfterAs(types.CommandID(cmd), node, fmt.Errorf("the gate could not read its results: %s", v.Why), commit)
	} else {
		err = r.commandAs(r.ctx, types.CommandID(cmd), func(tx *eventstore.Tx) ([]eventstore.Event, error) { // past the bound's work: its deadline cuts no record
			return nil, commit(tx)
		})
	}
	if err != nil { // the commit refused: held while its claim holds (Astra chunk 1 M1)
		return errors.Join(err, r.holdClaimed(node, g.view.State.Key, err))
	}
	return nil
}

// redBase reports whether r, a base's measured report, is red: a test or
// a package failed, a package that failed to build with no test result
// included (Astra chunk 1 M2). A report with neither (no results, or a run
// that did not end on its own) is no red base: its measure is retried.
func redBase(r results.Report) bool {
	return slices.ContainsFunc(r.Results, func(x results.Result) bool { return x.Outcome == results.Fail }) ||
		slices.Contains(slices.Collect(maps.Values(r.Packages)), results.Fail)
}

// gating reads what node's gate run takes (gating's fields): its claim's
// one input, accept's accepted tree, and what accept took.
func (r *runner) gating(ctx context.Context, node types.NodeID) (gating, error) {
	var g gating
	var acceptNode types.NodeID
	err := r.store.View(ctx, func(tx *eventstore.Tx) error {
		var err error
		if g.view, err = graph.ViewOf(tx, node); err != nil {
			return err
		}
		if g.view.Node.Kind != types.NodeKindGate || g.view.State.State != types.NodeStateStateClaimed {
			return fmt.Errorf("node %s is no claimed gate node", node)
		}
		ins, _, err := graph.ClaimInputs(tx, node)
		if err != nil {
			return err
		}
		if len(ins) != 1 || ins[0].Result == nil {
			return fmt.Errorf("%w: gate node %s takes %d inputs, not one accepted tree", errContent, node, len(ins))
		}
		raw, ok, err := tx.Get(graph.ResultProjection, string(*ins[0].Result))
		var rr types.ResultRecorded
		if err == nil && ok {
			err = json.Unmarshal(raw, &rr)
		}
		switch {
		case err != nil:
			return err
		case !ok || rr.Attempt != nil || rr.Result.Kind != types.ResultKindTree || len(rr.Result.Value) == 0 || rr.Result.Value[0].Type != types.RefTypeGitTree:
			return fmt.Errorf("%w: gate node %s's input is no tree accept recorded", errContent, node)
		}
		g.tree, acceptNode = rr.Result.Value[0].ID, rr.Node
		return nil
	})
	if err != nil {
		return g, err
	}
	g.accept, err = r.acceptance(ctx, acceptNode, false)
	return g, err
}

// changedFiles are the paths that differ between the base tree and tree,
// in repo (dir the clone's parent, git's work tree, which diff never
// reads).
func changedFiles(ctx context.Context, repo sandbox.Repo, dir, base, tree string) ([]string, error) {
	cmd, err := repo.Git(ctx, dir, "diff", "--name-only", "-z", "--no-renames", base, tree)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git diff of the accepted tree: %w", err)
	}
	var paths []string
	for p := range strings.SplitSeq(strings.TrimSuffix(out.String(), "\x00"), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}
