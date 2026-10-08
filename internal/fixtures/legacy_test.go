// SPDX-License-Identifier: Apache-2.0

package fixtures

// The legacy golden loader (docs/design/m0/11 "Legacy incident goldens";
// plan 15 L1, O6). It loads each golden of fixtures/legacy/ and verifies its
// setup before any step runs: golden.json decodes strictly; its provenance
// (the predecessor path it names, the anonymisation) holds; every artifact
// has the shape the loader reads; and no trimming removed a terminal record
// or a decision input. The M0 steps then run through the real components;
// a step another milestone owns is setup-checked and listed with it.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab/exitmap"
)

const legacyRoot = "../../fixtures/legacy"

// legacyGolden is a golden.json (fixtures/legacy/README.md "How to read").
type legacyGolden struct {
	Source       string          `json:"source"`
	Class        string          `json:"class"`
	DesignRefs   []string        `json:"design_refs"`
	Inputs       json.RawMessage `json:"inputs"`
	Expected     []Step          `json:"expected"`
	Variants     json.RawMessage `json:"variants"`
	ReasonStatus string          `json:"reason_status"`
	Notes        string          `json:"notes"`
}

// shape is one class of artifact: the files matching glob (count of them),
// the fields each must carry (dotted paths, * for every array item), and,
// if set, the array holding one item per expected step.
type shape struct {
	glob    string
	count   int
	fields  []string
	perStep string
}

// legacySetup is what a golden's loader reads, and the milestone that owns
// each expected step (index.md rows A3, A4, A6, B21).
type legacySetup struct {
	shapes []shape
	owners []string
}

var walkFields = []string{"identity.head", "identity.tree", "identity.grant_sha256", "status", "result.walk.status", "result.walk.plan_sha256",
	"result.walk.walked.tree", "result.walk.walked.plan_sha256", "result.walk.planned", "result.walk.observed", "result.walk.disagreements"}

var legacySetups = map[string]legacySetup{
	"f16-walk-demand": {shapes: []shape{
		{glob: "packet-*.json", count: 2, fields: []string{"head", "base", "tree", "input_digest", "task.id", "evidence_jobs.*.id", "evidence_jobs.*.path", "evidence_jobs.*.result.status"}},
		{glob: "walk-*.json", count: 3, fields: walkFields},
	}, owners: []string{"M0", "M1", "M0", "M0"}}, // step 2: an evidence run yields evidence_observed only once the graph is wired (15 §2 L1)
	"f16-inline-fallback": {shapes: []shape{
		{glob: "fallback-primary-usage.json", count: 1, fields: []string{"call", "completed", "terminal_error.message", "model_items", "tool_events", "mode"}},
		{glob: "rollout-*.jsonl", count: 1},
		{glob: "usage.json", count: 1, fields: []string{"call", "completed", "route.kind", "route.model", "route.authorization", "route.primary_stopped_with"}},
		{glob: "packet.json", count: 1, fields: []string{"mode", "task.id", "head"}},
		{glob: "session.json", count: 1, fields: []string{"mode", "head", "input_digest"}},
		{glob: "budget.json", count: 1, fields: []string{"task_id", "runs", "charged_calls"}},
		{glob: "intent", count: 1},
	}, owners: []string{"M0", "M2"}}, // step 2: fallback_scope placement
	"edge-owned-errors": {shapes: []shape{{glob: "edge-*.log", count: 1}}, owners: []string{"M1", "M1"}},
	"closure-record-finding-r4": {shapes: []shape{{glob: "closure-record-finding-r4.json", count: 1,
		fields: []string{"source", "findings.*.severity", "findings.*.surface", "findings.*.fix"}, perStep: "findings"}}, owners: []string{"M1", "M1", "M1"}},
}

// legacy is a loaded golden: its golden.json and its artifacts by name.
type legacy struct {
	golden legacyGolden
	files  map[string][]byte
}

// file is the one artifact matching glob.
func (l legacy) file(glob string) []byte {
	for name, b := range l.files {
		if ok, _ := path.Match(glob, name); ok {
			return b
		}
	}
	return nil
}

var (
	hexRun  = regexp.MustCompile(`[0-9a-f]{40,}`)
	uuidRun = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
)

// loadLegacy loads golden name from root and verifies its setup.
func loadLegacy(root, name string) (legacy, error) {
	setup, ok := legacySetups[name]
	if !ok {
		return legacy{}, fmt.Errorf("%s: no setup is declared for it", name)
	}
	entries, err := os.ReadDir(filepath.Join(root, name))
	if err != nil {
		return legacy{}, err
	}
	l := legacy{files: map[string][]byte{}}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(root, name, e.Name()))
		if err != nil {
			return legacy{}, err
		}
		l.files[e.Name()] = b
	}
	raw := l.files["golden.json"]
	delete(l.files, "golden.json")
	if _, err := contract.ReadStrict(raw); err != nil {
		return legacy{}, fmt.Errorf("%s/golden.json: %w", name, err)
	}
	if err := contract.UnmarshalStrict(raw, &l.golden); err != nil {
		return legacy{}, fmt.Errorf("%s/golden.json: %w", name, err)
	}
	g := l.golden
	if g.Source == "" || g.Class == "" || g.Notes == "" || g.ReasonStatus == "" || len(g.DesignRefs) == 0 || len(g.Expected) == 0 ||
		slices.ContainsFunc(g.Expected, func(s Step) bool { return s.Outcome == "" || s.Reason == "" }) {
		return legacy{}, fmt.Errorf("%s/golden.json: a required field is missing", name)
	}
	if len(setup.owners) != len(g.Expected) {
		return legacy{}, fmt.Errorf("%s: %d expected steps, %d with an owner", name, len(g.Expected), len(setup.owners))
	}
	// Provenance: a predecessor fixture path naming this directory or one of
	// its artifacts, and the README's anonymisation everywhere.
	src, dir := strings.CutSuffix(g.Source, "/")
	if base := path.Base(src); !strings.HasPrefix(g.Source, "t/fixtures/") || dir && base != name || !dir && l.files[base] == nil {
		return legacy{}, fmt.Errorf("%s: source %q names neither it nor one of its artifacts", name, g.Source)
	}
	all := maps.Clone(l.files)
	all["golden.json"] = raw
	for f, b := range all {
		for _, h := range append(hexRun.FindAllString(string(b), -1), uuidRun.FindAllString(string(b), -1)...) {
			if !strings.HasPrefix(h, "5eed") && !strings.HasPrefix(h, "00000000-0000-4000-8000-") {
				return legacy{}, fmt.Errorf("%s/%s: %s is not anonymised", name, f, h)
			}
		}
	}
	matched := 0
	for _, s := range setup.shapes {
		n := 0
		for f, b := range l.files {
			if ok, _ := path.Match(s.glob, f); ok {
				n++
				if err := s.check(b, len(g.Expected)); err != nil {
					return legacy{}, fmt.Errorf("%s/%s: %w", name, f, err)
				}
			}
		}
		if n != s.count {
			return legacy{}, fmt.Errorf("%s: %d artifacts match %s, want %d", name, n, s.glob, s.count)
		}
		matched += n
	}
	if matched != len(l.files) {
		return legacy{}, fmt.Errorf("%s: an artifact matches no declared shape", name)
	}
	return l, nil
}

// check is the shape's verdict on one artifact; steps is the golden's
// expected length.
func (s shape) check(b []byte, steps int) error {
	switch {
	case strings.HasSuffix(s.glob, ".jsonl"):
		return terminalStream(b)
	case strings.HasSuffix(s.glob, ".log"):
		return accountedLog(b)
	case !strings.HasSuffix(s.glob, ".json"):
		return nil
	}
	doc, err := readDoc(b)
	if err != nil {
		return err
	}
	for _, f := range s.fields {
		if !has(doc, strings.Split(f, ".")) {
			return fmt.Errorf("the field %s is missing", f)
		}
	}
	if s.perStep != "" {
		if items, _ := doc.(map[string]any)[s.perStep].([]any); len(items) != steps {
			return fmt.Errorf("%d %s, %d expected steps", len(items), s.perStep, steps)
		}
	}
	return nil
}

// readDoc decodes one captured JSON value with nothing after it. Captures
// keep the producer's fractional numbers, which the contract reader
// refuses, so it is not used for them.
func readDoc(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("data after the JSON value")
	}
	return doc, nil
}

// has: doc holds the path; * is every item of a non-empty array.
func has(doc any, p []string) bool {
	if len(p) == 0 {
		return true
	}
	if p[0] == "*" {
		items, ok := doc.([]any)
		return ok && len(items) > 0 && !slices.ContainsFunc(items, func(i any) bool { return !has(i, p[1:]) })
	}
	m, ok := doc.(map[string]any)
	v, ok2 := m[p[0]]
	return ok && ok2 && has(v, p[1:])
}

// terminalStream: a rollout's events in ordinal order, its terminal record
// (task_complete) last; a capture without one is a truncation (11:158).
func terminalStream(b []byte) error {
	last, kind := int64(-1), ""
	for i, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		var ev struct {
			Ordinal *int64 `json:"ordinal"`
			Payload struct {
				Type string `json:"type"`
			} `json:"payload"`
		}
		if _, err := readDoc([]byte(line)); err != nil {
			return fmt.Errorf("line %d: %w", i+1, err)
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Ordinal == nil || *ev.Ordinal <= last || ev.Payload.Type == "" {
			return fmt.Errorf("line %d: no event in ordinal order", i+1)
		}
		last, kind = *ev.Ordinal, ev.Payload.Type
	}
	if kind != "task_complete" {
		return errors.New("the stream's terminal record (task_complete) was trimmed")
	}
	return nil
}

var summaryCount = regexp.MustCompile(`\b(\d+) (failed|errors?)\b`)

// accountedLog: a gate log ends with its summary line, and the short
// summary lists exactly the failures and errors it counts (07).
func accountedLog(b []byte) error {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	summary := lines[len(lines)-1]
	listed := map[string]int{}
	for _, l := range lines {
		if k, _, _ := strings.Cut(l, " "); k == "FAILED" || k == "ERROR" {
			listed[k]++
		}
	}
	counted := map[string]int{}
	for _, m := range summaryCount.FindAllStringSubmatch(summary, -1) {
		n, _ := strconv.Atoi(m[1])
		counted[map[bool]string{true: "FAILED", false: "ERROR"}[m[2] == "failed"]] = n
	}
	if !strings.Contains(summary, " passed") || counted["FAILED"] != listed["FAILED"] || counted["ERROR"] != listed["ERROR"] {
		return fmt.Errorf("the summary %q does not account for %v listed", summary, listed)
	}
	return nil
}

// legacyRunners run a golden's M0 steps, in order, through the real
// components.
var legacyRunners = map[string]func(*testing.T, legacy) ([]Step, []string, error){
	"f16-walk-demand":     walkDemandSteps,
	"f16-inline-fallback": inlineFallbackSteps,
}

// demandKey is the golden's demand_key_assumption: H(plan_sha256, subject).
func demandKey(plan, tree string) types.Digest {
	sum := sha256.Sum256([]byte(plan + "\x00" + tree))
	return types.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// walkDemandSteps: the review the requesting packet records used the old
// plan's walk. Offered for the new demand, that receipt is inapplicable
// (step 1, internal/receipt through graph.Resolve); against the new plan's
// walk, G5's equivalence re-runs the review (step 3); against the other walk
// of the old plan it does not (step 4). A walk is a receipt with one clean
// predicate per observed step, about the walked tree under its demand.
func walkDemandSteps(t *testing.T, l legacy) ([]Step, []string, error) {
	var in struct {
		Demands map[string]struct {
			Plan      string   `json:"plan_sha256"`
			Requester string   `json:"requested_by_packet"`
			Receipts  []string `json:"receipts"`
		} `json:"demands"`
	}
	var packet struct {
		Jobs []struct{ ID, Path string } `json:"evidence_jobs"`
	}
	err := json.Unmarshal(l.golden.Inputs, &in)
	old, fresh := in.Demands["old"], in.Demands["new"]
	if err == nil {
		err = json.Unmarshal(l.file(fresh.Requester), &packet)
	}
	i := slices.IndexFunc(packet.Jobs, func(j struct{ ID, Path string }) bool { return j.ID == "browser-walk" })
	if err != nil || len(old.Receipts) != 2 || len(fresh.Receipts) != 1 || i < 0 {
		return nil, nil, errors.Join(err, errors.New("inputs.demands names no old pair, new walk, and requesting packet with its walk"))
	}
	used := "walk-" + path.Base(packet.Jobs[i].Path)
	if !slices.Contains(old.Receipts, used) {
		return nil, nil, fmt.Errorf("the requesting packet's walk %s is not of the old demand", used)
	}
	g, closeFn, err := openGraph(t.Context(), t.TempDir())
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = closeFn() }()
	receipt := func(name, plan string) (ref types.Ref, c json.RawMessage, tree string, err error) {
		var w struct {
			Result struct {
				Walk struct {
					Plan          string                `json:"plan_sha256"`
					Walked        struct{ Tree string } `json:"walked"`
					Observed      int                   `json:"observed"`
					Disagreements []json.RawMessage     `json:"disagreements"`
				} `json:"walk"`
			} `json:"result"`
		}
		if err = json.Unmarshal(l.file(name), &w); err != nil || w.Result.Walk.Plan != plan || len(w.Result.Walk.Disagreements) > 0 {
			return ref, nil, "", errors.Join(err, fmt.Errorf("%s is no clean walk of plan %s", name, plan))
		}
		var ps []types.Predicate
		for s := range w.Result.Walk.Observed {
			ps = append(ps, types.Predicate{Name: fmt.Sprintf("step-%02d", s+1), Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Evidence: []types.PredicateEvidenceItem{}})
		}
		tree = "git:" + w.Result.Walk.Walked.Tree
		ref, c, err = g.gateReceiptWith(tree, demandKey(plan, tree), nil, []types.Restriction{}, ps...)
		return ref, c, tree, err
	}
	ref, c, tree, err := receipt(used, old.Plan)
	var gate, review types.NodeID
	var usedRes types.ResultID
	if err == nil {
		gate, usedRes, err = g.complete(types.NodeKindGate, json.RawMessage(`{}`), on("plan", g.plan, types.ResultKindPlan),
			map[string]graph.Resolved{"plan": {Result: g.planRes}}, types.ResultKindGateResult, []types.Ref{ref}, &c)
	}
	if err == nil {
		review, err = g.reviewOf(gate)
	}
	var o observe
	if err == nil {
		var st Step
		st, err = g.resolved(review, tree, demandKey(fresh.Plan, tree), receiptsAt)
		o.steps = append(o.steps, st)
	}
	env := g.env(tree, demandKey(old.Plan, tree))
	if err == nil {
		err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Evaluate(tx, []types.NodeID{review}, env)
		})
	}
	if err == nil {
		var st Step
		st, err = g.commit(review, types.ResultKindReview, env)
		o.check(st == cleanStep, "the review on the old walk did not commit: %s", st)
	}
	other := old.Receipts[1-slices.Index(old.Receipts, used)]
	for _, w := range []struct{ name, plan string }{{fresh.Receipts[0], fresh.Plan}, {other, old.Plan}} {
		var now types.ResultID
		same := false
		if err == nil {
			ref, c, _, err = receipt(w.name, w.plan)
		}
		if err == nil {
			err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Invalidate(tx, gate) })
		}
		if err == nil {
			now, err = g.selectResult(gate, types.ResultKindGateResult, []types.Ref{ref}, &c)
		}
		if err == nil {
			err = g.s.View(t.Context(), func(tx *eventstore.Tx) (err error) {
				same, err = graph.Equivalent(tx, review, "gate", usedRes, now, g.blobs)
				return err
			})
		}
		o.steps = append(o.steps, map[bool]Step{false: changedStep, true: {vocab.Empty, vocab.ReasonConclusionsUnchanged.String()}}[same])
	}
	return o.steps, o.violations, err
}

// inlineFallbackSteps: the primary review attempt, from the controller's
// facts in the capture (no accepted result, nothing changed) and the
// stream's error, classified as an adapter's class_hint would be, goes
// through G2's outcome table and the exit map: could_not_run with the
// credit reason, an attempt's record, never an empty review (step 1).
func inlineFallbackSteps(t *testing.T, l legacy) ([]Step, []string, error) {
	var in struct {
		Lease struct{ Role, Mode string } `json:"lease"`
	}
	var usage struct {
		Completed  bool `json:"completed"`
		ModelItems int  `json:"model_items"`
		ToolEvents int  `json:"tool_events"`
	}
	err := errors.Join(json.Unmarshal(l.golden.Inputs, &in), json.Unmarshal(l.file("fallback-primary-usage.json"), &usage))
	if err != nil || usage.ModelItems+usage.ToolEvents > 0 {
		return nil, nil, errors.Join(err, errors.New("the primary attempt did work the capture does not carry"))
	}
	var exit exitmap.Exit
	for line := range strings.Lines(string(l.file("rollout-*.jsonl"))) {
		var ev struct {
			Payload struct {
				RateLimits *struct {
					Credits *struct {
						HasCredits bool `json:"has_credits"`
					} `json:"credits"`
				} `json:"rate_limits"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, nil, err
		}
		if rl := ev.Payload.RateLimits; rl != nil && rl.Credits != nil && !rl.Credits.HasCredits {
			exit.Hints = append(exit.Hints, "credit_exhausted")
		}
	}
	nid, err := ids.New("nod")
	params, perr := json.Marshal(map[string]string{"mode": in.Lease.Mode})
	if err = errors.Join(err, perr); err != nil {
		return nil, nil, err
	}
	n := types.Node{ID: types.NodeID(nid), Kind: types.NodeKindReview, DefinitionVersion: "1", Round: 1, Params: params, Inputs: []types.InputSpec{},
		Requires: types.NodeRequires{Role: ptr(types.Name(in.Lease.Role)), Capability: ptr(types.Name("review")), Resources: []types.ResourceGroupKind{types.ResourceGroupKindAccountQuota}}}
	key, err := graph.Key(n, map[string]graph.Resolved{}, 1)
	var r types.Result
	if err == nil {
		r, err = graph.Build(key, graph.BuildFacts{Project: gProject, Submitted: usage.Completed, Exit: exit})
	}
	var o observe
	o.steps = append(o.steps, Step{vocab.Outcome(r.Outcome), r.Reason.String()})
	o.check(graph.Check(r) == nil, "not a valid records/result: %v", graph.Check(r))
	o.check(!r.Completed && len(r.Value) == 0, "the attempt's record reads as finished work: %+v", r)
	return o.steps, o.violations, err
}

func legacyNames(t *testing.T) []string {
	entries, err := os.ReadDir(legacyRoot)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) != len(legacySetups) {
		t.Fatalf("goldens %v, setups declared for %d", names, len(legacySetups))
	}
	return names
}

// Every golden's setup holds; its M0 steps yield their golden steps through
// the real components, and the others are listed with their milestone.
func TestLegacyGoldens(t *testing.T) {
	for _, name := range legacyNames(t) {
		t.Run(name, func(t *testing.T) {
			l, err := loadLegacy(legacyRoot, name)
			if err != nil {
				t.Fatal(err)
			}
			var want []Step
			for i, owner := range legacySetups[name].owners {
				if owner == "M0" {
					want = append(want, l.golden.Expected[i])
				} else {
					t.Logf("step %d %s: setup-checked, owned by %s", i+1, l.golden.Expected[i], owner)
				}
			}
			if len(want) == 0 {
				return
			}
			if legacyRunners[name] == nil {
				t.Fatalf("%d M0 steps and no runner", len(want))
			}
			got, violations, err := legacyRunners[name](t, l)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) || len(violations) > 0 {
				t.Errorf("got %s, want %s; %v", steps(got), steps(want), violations)
			}
		})
	}
}

// The loader refuses a mutated artifact: each mutation of a copy is refused.
func TestLegacyLoaderRefusesMutations(t *testing.T) {
	drop := func(field string) func(string) string {
		return func(s string) string {
			return regexp.MustCompile(`(?m)^\s*"`+field+`": [^\n]*\n`).ReplaceAllString(s, "")
		}
	}
	cases := map[string]struct {
		golden, file, want string
		edit               func(string) string
	}{
		"a trimmed terminal record": {"f16-inline-fallback", "rollout-*.jsonl", "task_complete", func(s string) string {
			return s[:strings.LastIndex(strings.TrimSuffix(s, "\n"), "\n")+1]
		}},
		"a missing walk field":    {"f16-walk-demand", "walk-1790945582297587810.json", "result.walk.plan_sha256", drop("plan_sha256")},
		"a missing golden field":  {"closure-record-finding-r4", "golden.json", "required field", drop("class")},
		"an unknown golden field": {"edge-owned-errors", "golden.json", "unknown field", func(s string) string { return strings.Replace(s, "{", `{"extra": 1,`, 1) }},
		"an unaccounted error":    {"edge-owned-errors", "edge-*.log", "does not account", func(s string) string { return strings.Replace(s, "ERROR tests/", "", 1) }},
		"a trimmed finding": {"closure-record-finding-r4", "closure-record-finding-r4.json", "expected steps", func(s string) string {
			var d map[string]any
			if json.Unmarshal([]byte(s), &d) != nil {
				return s
			}
			d["findings"] = d["findings"].([]any)[1:]
			b, _ := json.Marshal(d)
			return string(b)
		}},
		"a digest not anonymised": {"f16-inline-fallback", "session.json", "not anonymised", func(s string) string { return strings.Replace(s, `"5eed0003`, `"abcd0003`, 1) }},
		"a source that is not here": {"f16-walk-demand", "golden.json", "names neither", func(s string) string {
			return strings.Replace(s, "t/fixtures/f16-walk-demand/", "t/fixtures/other/", 1)
		}},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(legacyRoot, c.golden)
			entries, err := os.ReadDir(src)
			if err != nil || os.Mkdir(filepath.Join(root, c.golden), 0o700) != nil {
				t.Fatal(err)
			}
			edited := false
			for _, e := range entries {
				b, err := os.ReadFile(filepath.Join(src, e.Name()))
				if err != nil {
					t.Fatal(err)
				}
				if ok, _ := path.Match(c.file, e.Name()); ok {
					after := c.edit(string(b))
					edited, b = after != string(b), []byte(after)
				}
				if err := os.WriteFile(filepath.Join(root, c.golden, e.Name()), b, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err = loadLegacy(root, c.golden)
			if !edited || err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("edited %v; loaded with %v, want a refusal naming %q", edited, err, c.want)
			}
		})
	}
}
