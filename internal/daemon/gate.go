// SPDX-License-Identifier: Apache-2.0

package daemon

// The gate run (P22a-5c-2b-3; W15, I13): the controller runs a claimed gate
// node itself, no model, no lease. The loop decides Gate for it
// (reconcile.Gate); the runner starts one goroutine per node, never two:
//  1. what the node takes, read in one snapshot: its claim's one input,
//     accept's result (an accepted tree, recorded with no attempt), and,
//     through accept's node, what accept took (acceptance): the build's
//     patch at its attempt's base tree; and the task's acceptance tests,
//     its record's (#226 entry 190);
//  2. a fresh clone of the project's repository, and the accepted tree in
//     it: accept.Accept again over the base and the patch, without its
//     guard, whose tree must be the accepted one (the tree is content
//     addressed: another is the log's inconsistency, refused);
//  3. the baseline, measured on the base tree (gate.Measure) with no
//     overlay (a hidden test fails at the base by design), and the gate
//     run on the accepted tree against it (gate.Run) with the task's
//     acceptance tests overlaid (§2.3 assertion 2, §2.6), each a bounded
//     sandbox run in the profile's one pinned image, in the profile's one
//     gate environment, under gateLimits; the changed files are the base's
//     and the tree's diff. Each acceptance test is read from the blob
//     store and must hash to its record's digest (readBlob: a read that
//     fails is the host's, a bounded retry), and its path, the record's
//     repository path, must be no path the profile protects
//     (accept.Policy) and none the base holds, nor under a file it holds:
//     a hidden test never replaces a visible file; nor one go test ./...
//     never runs (overlayOf). A path the candidate added is replaced (the
//     hidden test is the task's). A refusal is the task's input, no retry
//     fixes it (errContent, "runner: failed"). A hidden test's result is
//     only as sound as accept's forgery guard (Opus #429 L2): the gate
//     reads the stream as written, and gates only the tree accept took;
//  4. its result (gate.Result: the receipt, the report and the restricted
//     results), recorded with no attempt (graph.Commit) by the command the
//     receipt names, derived from the acceptance blobs it overlaid (their
//     lineage: the builder never takes it; the reviewer takes it
//     sanitised). The receipt and the report carry results, never the
//     acceptance's bytes.
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
// the next start runs the gate again (stated, Opus 22a-5c-2b-3 L2). The
// loop decides a Gate only while launch is active (this build, P22a).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/accept"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gate"
	"github.com/ekaynac/auto-dev-cycle/internal/gosel"
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
	view       graph.NodeView
	tree       string // the accepted tree, hex
	accept     acceptance
	acceptance []types.AcceptanceBlob // the task's, its record's
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
	overlay, digests, err := r.overlayOf(ctx, repo, dir, g.accept.base, policy, prof.Gate, g.acceptance)
	if err != nil {
		return errors.Join(err, r.hold(node, err))
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
	in.Tree, in.Baseline, in.Overlay = g.tree, &base, overlay
	v, err := gate.Run(ctx, in)
	switch {
	case errors.Is(err, gate.ErrAcceptance): // overlayOf refused it already: a backstop, the task's
		err = fmt.Errorf("%w: %w", errContent, err)
		return errors.Join(err, r.hold(node, err))
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
		res, err := gate.Result(*cur.State.Key, v, contract, img, gate.Subject{Project: g.view.Project, Revision: buildContractRevision, Acceptance: digests},
			types.CommandID(cmd), r.blobs, tx.Now())
		if err != nil {
			return err
		}
		if err := graph.Commit(tx, node, res, r.env); err != nil {
			return err
		}
		ev, err := gate.RecordMeasured(node, res.ID, base, img) // what it was measured against: a consumer's requirement derives from it (P22a-6a′)
		if err != nil {
			return err
		}
		return tx.Emit(ev)
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
		t, err := authority.TaskOf(tx, g.view.Task)
		g.acceptance = t.Acceptance
		return err
	})
	if err != nil {
		return g, err
	}
	g.accept, err = r.acceptance(ctx, acceptNode, false)
	return g, err
}

// overlayOf is the gate run's acceptance overlay: each of the task's
// acceptance tests at its record's repository path, its bytes the blob
// store's checked against its digest (at most an upload's bound, and
// gate.MaxOverlay in all, before the base's measure: Astra #429 M5), and
// their digests, the result's lineage. Refused as the task's (errContent,
// "runner: failed": no retry fixes it), before any run: a path the
// profile protects; one the base tree holds, or under a file it holds; one
// in a module the base nests (a go.mod in a directory between it and the
// root: go test ./... never runs it); a base with no root go.mod; and an
// acceptance go test ./... could never run on the gate environment's
// platform (gate.AcceptanceTests: Opus #429 H1, M1, L1; Astra #429 H1, M4),
// or with a Go file, test or helper, that can never compile in the base's
// package it joins (gate.AcceptanceFits: another package clause, a name a
// file of that package declares; N1, Astra H2). These last are a
// go_test_json gate's; a gate of another adapter is the profile's, which
// gate.Measure refuses (gate.ErrSpec). A blob the store
// cannot read, missing or not its content, is the host's (a plain error, a
// bounded retry, as accept's own reads: Opus #429 M2): the upload was
// digest-checked when it was published. Every error is a fixed text (Astra
// #429 H3).
func (r *runner) overlayOf(ctx context.Context, repo sandbox.Repo, dir, base string, policy accept.Policy, g types.ProjectProfileGate,
	acc []types.AcceptanceBlob) (map[string][]byte, []types.Digest, error) {
	goTest := g.ResultAdapter == results.GoTestJSON
	overlay, digests := map[string][]byte{}, make([]types.Digest, 0, len(acc))
	if len(acc) == 0 {
		return nil, digests, nil
	}
	var pl [2]string
	if len(g.Environments) == 1 {
		pl = [2]string{g.Environments[0].Facts.Runtime.Os, g.Environments[0].Facts.Runtime.Arch}
	}
	goTest = goTest && len(g.Environments) == 1
	// What the base must hold, asked in one batch: a root go.mod (a blob);
	// nothing at each path; at each of its directories nothing or a tree,
	// and no go.mod in one below the root.
	type ask struct {
		at, of string
		ok     func(kind string) bool
	}
	var asks []ask
	total := 0
	if goTest {
		asks = append(asks, ask{at: "go.mod", ok: func(k string) bool { return k == "blob" }})
	}
	for _, a := range acc {
		p := a.Name
		if policy.Protected(p) {
			return nil, nil, fmt.Errorf("%w: %w", errContent, errAcceptanceProtected)
		}
		asks = append(asks, ask{p, p, func(k string) bool { return k == "missing" }})
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			asks = append(asks, ask{d, p, func(k string) bool { return k == "missing" || k == "tree" }})
			if goTest {
				asks = append(asks, ask{d + "/go.mod", p, func(k string) bool { return k == "missing" }})
			}
		}
		b, err := r.readBlob(a.Digest, blobstore.MaxUploadBytes)
		if err != nil {
			return nil, nil, errAcceptanceRead // the host's: its cause names the blob (Astra #429 H3)
		}
		if total += len(b); total > gate.MaxOverlay { // before the base's measure (Astra #429 M5)
			return nil, nil, fmt.Errorf("%w: %w", errContent, errAcceptanceLarge)
		}
		overlay[p], digests = b, append(digests, a.Digest)
	}
	if goTest {
		if _, err := gate.AcceptanceTests(pl, overlay); err != nil {
			return nil, nil, fmt.Errorf("%w: %w", errContent, err)
		}
	}
	var check strings.Builder
	for _, q := range asks {
		fmt.Fprintf(&check, "%s:%s\n", base, q.at)
	}
	cmd, err := repo.Git(ctx, dir, "cat-file", "--batch-check=%(objecttype)")
	if err != nil {
		return nil, nil, err
	}
	var out bytes.Buffer
	cmd.Stdin, cmd.Stdout = strings.NewReader(check.String()), &out
	if err := cmd.Run(); err != nil {
		return nil, nil, fmt.Errorf("git cat-file of the base's acceptance paths: %w", err)
	}
	kinds := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(kinds) != len(asks) {
		return nil, nil, fmt.Errorf("git cat-file answered %d of %d paths", len(kinds), len(asks))
	}
	for i, q := range asks {
		k := kinds[i]
		if strings.HasSuffix(k, " missing") {
			k = "missing"
		}
		switch {
		case q.ok(k):
		case q.of == "":
			return nil, nil, fmt.Errorf("%w: the base has no root go.mod to place the acceptance tests in", errContent)
		case strings.HasSuffix(q.at, "/go.mod"):
			return nil, nil, fmt.Errorf("%w: an acceptance file is in a module the base nests, which go test ./... never runs", errContent)
		default:
			return nil, nil, fmt.Errorf("%w: an acceptance file would replace what the base holds", errContent)
		}
	}
	if goTest {
		// The base's packages the acceptance's Go files join: one that can
		// never compile there is the task's (Opus #429 N1, Astra H2).
		trees := map[string]bool{".": true}
		for i, q := range asks {
			trees[q.at] = trees[q.at] || kinds[i] == "tree"
		}
		files, err := basePackages(ctx, repo, dir, base, trees, overlay)
		if err != nil {
			return nil, nil, err
		}
		if err := gate.AcceptanceFits(pl, overlay, files); err != nil {
			return nil, nil, fmt.Errorf("%w: %w", errContent, err)
		}
	}
	return overlay, digests, nil
}

// The acceptance's refusals and its read failure, each a fixed text: none
// names an acceptance path, an identifier, a blob digest or a store path
// (Astra #429 H3), so adcd's log carries none.
var (
	errAcceptanceProtected = errors.New("an acceptance file is at a path the profile protects")
	errAcceptanceLarge     = errors.New("the acceptance's files are over the overlay's bound")
	errAcceptanceRead      = errors.New("daemon: an acceptance blob could not be read from the blob store")
)

// maxBasePackages bounds the base's Go files basePackages reads.
const maxBasePackages = 16 << 20

// basePackages are the base's root go.mod and its Go files (repository
// path → bytes) in each directory an active Go file of overlay is in that
// the base holds as a tree (trees), read with cat-file: at most
// maxBasePackages bytes, more the task's (errContent).
func basePackages(ctx context.Context, repo sandbox.Repo, dir, base string, trees map[string]bool, overlay map[string][]byte) (map[string][]byte, error) {
	git := func(stdin string, args ...string) ([]byte, error) {
		cmd, err := repo.Git(ctx, dir, args...)
		if err != nil {
			return nil, err
		}
		var out bytes.Buffer
		cmd.Stdin, cmd.Stdout = strings.NewReader(stdin), &out
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("git %s of the base's packages: %w", args[0], err)
		}
		return out.Bytes(), nil
	}
	paths, objects, listed := []string{"go.mod"}, []string{base + ":go.mod"}, map[string]bool{} // the root go.mod: its modules (gate.AcceptanceFits)
	for _, p := range slices.Sorted(maps.Keys(overlay)) {
		d := path.Dir(p)
		if !gosel.Active(p) || !trees[d] || listed[d] {
			continue
		}
		listed[d] = true
		spec := base
		if d != "." {
			spec += ":" + d
		}
		list, err := git("", "cat-file", "-p", spec)
		if err != nil {
			return nil, err
		}
		for line := range strings.Lines(string(list)) {
			meta, name, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "\t")
			f := strings.Fields(meta) // mode, type, object
			if ok && len(f) == 3 && (f[0] == "100644" || f[0] == "100755") && f[1] == "blob" && strings.HasSuffix(name, ".go") {
				paths, objects = append(paths, path.Join(d, name)), append(objects, f[2])
			}
		}
	}
	sizes, err := git(strings.Join(objects, "\n")+"\n", "cat-file", "--batch-check=%(objectsize)")
	if err != nil {
		return nil, err
	}
	total := 0
	for line := range strings.Lines(string(sizes)) {
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			return nil, fmt.Errorf("git cat-file of the base's packages: %q", line)
		}
		total += n
	}
	if total > maxBasePackages {
		return nil, fmt.Errorf("%w: the base's packages the acceptance tests join are over %d bytes", errContent, maxBasePackages)
	}
	out, err := git(strings.Join(objects, "\n")+"\n", "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, p := range paths {
		head, rest, ok := bytes.Cut(out, []byte("\n"))
		f := strings.Fields(string(head)) // object, type, size
		n := 0
		if len(f) == 3 {
			n, err = strconv.Atoi(f[2])
		}
		if !ok || len(f) != 3 || err != nil || n+1 > len(rest) {
			return nil, errors.New("git cat-file --batch answered fewer objects than asked")
		}
		files[p], out = rest[:n], rest[n+1:]
	}
	return files, nil
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
