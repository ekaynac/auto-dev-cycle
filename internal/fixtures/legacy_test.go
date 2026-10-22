// SPDX-License-Identifier: Apache-2.0

package fixtures

// The legacy golden loader (docs/design/m0/11 "Legacy incident goldens";
// plan 15 L1, O6). It loads each golden of fixtures/legacy/ and verifies its
// setup before any step runs: golden.json decodes strictly; its provenance
// (the predecessor path it names, the anonymisation) holds; every artifact
// has the shape the loader reads; no trimming removed a terminal record or a
// decision input; and the artifacts link to each other as the README says.
// The M0 steps then run through the real components; a step another
// milestone owns is setup-checked and listed with it.

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
	Source     string          `json:"source"`
	Class      string          `json:"class"`
	DesignRefs []string        `json:"design_refs"`
	Inputs     json.RawMessage `json:"inputs"`
	Expected   []Step          `json:"expected"`
	Variants   []struct {
		Change   string `json:"change"`
		Expected []Step `json:"expected"`
	} `json:"variants"`
	ReasonStatus string `json:"reason_status"`
	Notes        string `json:"notes"`
}

// shape is one class of artifact: the files matching glob (count of them),
// the fields each must carry (dotted paths, * for every array item; a
// present field is neither null nor ""), and, if set, the array holding
// one item per expected step.
type shape struct {
	glob    string
	count   int
	fields  []string
	perStep string
}

// legacySetup is what a golden's loader reads: its artifacts' shapes, the
// golden's own decision inputs and links (inputs), and the milestone that
// owns each expected step (index.md rows A3, A4, A6, B21).
type legacySetup struct {
	shapes []shape
	inputs func(legacy) error
	owners []string
}

var walkFields = []string{"identity.head", "identity.tree", "identity.grant_sha256", "status", "result.walk.status", "result.walk.plan_sha256",
	"result.walk.walked.tree", "result.walk.walked.plan_sha256", "result.walk.planned", "result.walk.observed", "result.walk.disagreements",
	"result.walk.review_event"}

var legacySetups = map[string]legacySetup{
	// Step 2's M0 part runs (the new demand's walk is not cap-exhausted);
	// its evidence_observed result is an evidence run's, which needs the
	// graph in adcd (15 §2 "The graph runs under fixtures only in M0").
	"f16-walk-demand": {shapes: []shape{
		{glob: "packet-*.json", count: 2, fields: []string{"head", "base", "tree", "input_digest", "task.id", "evidence_jobs.*.id", "evidence_jobs.*.path", "evidence_jobs.*.result.status"}},
		{glob: "walk-*.json", count: 3, fields: walkFields},
	}, inputs: walkDemandInputs, owners: []string{"M0", "M1", "M0", "M0"}},
	"f16-inline-fallback": {shapes: []shape{
		{glob: "fallback-primary-usage.json", count: 1, fields: []string{"call", "terminal_error.message", "model_items", "tool_events", "mode"}},
		{glob: "rollout-*.jsonl", count: 1},
		{glob: "usage.json", count: 1, fields: []string{"call", "route.kind", "route.model", "route.authorization", "route.primary_stopped_with"}},
		{glob: "packet.json", count: 1, fields: []string{"mode", "task.id", "head"}},
		{glob: "session.json", count: 1, fields: []string{"mode", "head", "input_digest"}},
		{glob: "budget.json", count: 1, fields: []string{"task_id", "runs", "charged_calls"}},
		{glob: "intent", count: 1},
	}, inputs: inlineFallbackInputs, owners: []string{"M0", "M2"}}, // step 2: fallback_scope placement
	"edge-owned-errors": {shapes: []shape{{glob: "edge-*.log", count: 1}}, inputs: edgeInputs, owners: []string{"M1", "M1"}},
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

// The README's anonymisation: digests and commit ids are 5eed plus a
// four-digit counter, zero-padded (epoch run ids, all decimal, are kept);
// UUIDs are 00000000-0000-4000-8000-<counter>; paths are under /home/user/;
// hosts are host-a.
var (
	hexRun    = regexp.MustCompile(`[0-9a-fA-F]{12,}`)
	synthetic = regexp.MustCompile(`^5eed[0-9a-f]{4}0*$`)
	uuidRun   = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	homeDir   = regexp.MustCompile(`/home/([^/\s"']+)`)
	hostName  = regexp.MustCompile(`(?:https?://|[\w.-]@)([A-Za-z][\w.-]*)`)
)

// anonymised: b holds no identifier the README's rules would have replaced.
func anonymised(b []byte) error {
	s := string(b)
	for _, h := range hexRun.FindAllString(s, -1) {
		if strings.ContainsAny(h, "abcdefABCDEF") && !synthetic.MatchString(h) {
			return fmt.Errorf("%s is not a synthetic digest", h)
		}
	}
	for _, u := range uuidRun.FindAllString(s, -1) {
		if !strings.HasPrefix(u, "00000000-0000-4000-8000-") {
			return fmt.Errorf("%s is not a synthetic UUID", u)
		}
	}
	for _, m := range append(homeDir.FindAllStringSubmatch(s, -1), hostName.FindAllStringSubmatch(s, -1)...) {
		if m[1] != "user" && m[1] != "host-a" {
			return fmt.Errorf("%s is not anonymised", m[0])
		}
	}
	return nil
}

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
		if !e.Type().IsRegular() {
			return legacy{}, fmt.Errorf("%s/%s is not a regular file", name, e.Name())
		}
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
		if err := anonymised(b); err != nil {
			return legacy{}, fmt.Errorf("%s/%s: %w", name, f, err)
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
	if setup.inputs != nil {
		if err := setup.inputs(l); err != nil {
			return legacy{}, fmt.Errorf("%s: %w", name, err)
		}
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
		_, err := accountedLog(b)
		return err
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

// has: doc holds the path, neither null nor ""; * is every item of a
// non-empty array.
func has(doc any, p []string) bool {
	if len(p) == 0 {
		return doc != nil && doc != ""
	}
	if p[0] == "*" {
		items, ok := doc.([]any)
		return ok && len(items) > 0 && !slices.ContainsFunc(items, func(i any) bool { return !has(i, p[1:]) })
	}
	m, ok := doc.(map[string]any)
	v, ok2 := m[p[0]]
	return ok && ok2 && has(v, p[1:])
}

// rolloutEvent is the part of a rollout event the loader reads.
type rolloutEvent struct {
	Ordinal *int64 `json:"ordinal"`
	Payload struct {
		Type       string `json:"type"`
		RateLimits *struct {
			Credits *struct {
				HasCredits *bool `json:"has_credits"`
			} `json:"credits"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

// noCredits: the event reports the account out of credits.
func (ev rolloutEvent) noCredits() bool {
	rl := ev.Payload.RateLimits
	return rl != nil && rl.Credits != nil && rl.Credits.HasCredits != nil && !*rl.Credits.HasCredits
}

func rollout(b []byte) ([]rolloutEvent, error) {
	var evs []rolloutEvent
	for i, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		var ev rolloutEvent
		if _, err := readDoc([]byte(line)); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Ordinal == nil || len(evs) > 0 && *ev.Ordinal <= *evs[len(evs)-1].Ordinal || ev.Payload.Type == "" {
			return nil, fmt.Errorf("line %d: no event in ordinal order", i+1)
		}
		evs = append(evs, ev)
	}
	return evs, nil
}

// terminalStream: a rollout's events in ordinal order, its terminal record
// (task_complete) last; a capture without one is a truncation (11:158).
func terminalStream(b []byte) error {
	evs, err := rollout(b)
	if err == nil && evs[len(evs)-1].Payload.Type != "task_complete" {
		err = errors.New("the stream's terminal record (task_complete) was trimmed")
	}
	return err
}

var summaryCount = regexp.MustCompile(`\b(\d+) (failed|errors?)\b`)

// accountedLog: a gate log ends with its summary line, and the short
// summary lists exactly the failures and errors it counts (07). It returns
// the counts.
func accountedLog(b []byte) (map[string]int, error) {
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
		return nil, fmt.Errorf("the summary %q does not account for %v listed", summary, listed)
	}
	return listed, nil
}

// edgeInputs: the incident's items, as the golden's notes count them: six
// setup ERRORs and one FAILED. A consistent trim of the log is refused too.
func edgeInputs(l legacy) error {
	listed, err := accountedLog(l.file("edge-*.log"))
	if err == nil && (listed["ERROR"] != 6 || listed["FAILED"] != 1) {
		err = fmt.Errorf("the log lists %v, the incident 6 ERROR and 1 FAILED", listed)
	}
	return err
}

// inlineFallbackInputs: step 1's decision inputs. The primary attempt has
// no accepted result and did no work, and its stream carries the credit
// event.
func inlineFallbackInputs(l legacy) error {
	var usage struct {
		Completed  *bool `json:"completed"`
		ModelItems int   `json:"model_items"`
		ToolEvents int   `json:"tool_events"`
	}
	if err := json.Unmarshal(l.file("fallback-primary-usage.json"), &usage); err != nil {
		return err
	}
	if usage.Completed == nil || *usage.Completed || usage.ModelItems+usage.ToolEvents > 0 {
		return errors.New("the primary attempt completed or did work: completed false, no model item and no tool event are its decision inputs")
	}
	evs, err := rollout(l.file("rollout-*.jsonl"))
	if err == nil && !slices.ContainsFunc(evs, rolloutEvent.noCredits) {
		err = errors.New("the rollout carries no credit event (has_credits false)")
	}
	return err
}

// walkDemand is f16-walk-demand's inputs and walks, as the loader reads them.
type walkDemand struct {
	old, fresh demandIn
	used       string // the old walk the requesting packet's review used
	tree       string
	walks      map[string]walkIn
}

type demandIn struct {
	Plan      string   `json:"plan_sha256"`
	Requester string   `json:"requested_by_packet"`
	Receipts  []string `json:"receipts"`
}

type walkIn struct {
	Identity struct{ Tree string } `json:"identity"`
	Result   struct {
		Walk struct {
			Plan          string                `json:"plan_sha256"`
			Walked        struct{ Tree string } `json:"walked"`
			Planned       int                   `json:"planned"`
			Observed      int                   `json:"observed"`
			Disagreements []json.RawMessage     `json:"disagreements"`
			ReviewEvent   string                `json:"review_event"`
		} `json:"walk"`
	} `json:"result"`
}

// readWalkDemand reads the golden's demands and checks their links (README
// "Anonymisation rules": a demanded walk carries its review packet's digest
// prefix): the two demands' plans differ; each walk walks its demand's plan
// in full, without disagreement, on the requesting packet's tree; the new
// walk's review_event ends in a prefix of that packet's input_digest; and
// the packet's browser-walk job is one of the old demand's walks.
func readWalkDemand(l legacy) (walkDemand, error) {
	var in struct {
		Demands map[string]demandIn `json:"demands"`
	}
	var packet struct {
		Tree        string                      `json:"tree"`
		InputDigest string                      `json:"input_digest"`
		Jobs        []struct{ ID, Path string } `json:"evidence_jobs"`
	}
	err := json.Unmarshal(l.golden.Inputs, &in)
	w := walkDemand{old: in.Demands["old"], fresh: in.Demands["new"], walks: map[string]walkIn{}}
	if err == nil {
		err = json.Unmarshal(l.file(w.fresh.Requester), &packet)
	}
	i := slices.IndexFunc(packet.Jobs, func(j struct{ ID, Path string }) bool { return j.ID == "browser-walk" })
	if err != nil || len(w.old.Receipts) != 2 || len(w.fresh.Receipts) != 1 || i < 0 || w.old.Plan == w.fresh.Plan {
		return w, errors.Join(err, errors.New("inputs.demands names no two plans, old pair, new walk, and requesting packet with its walk"))
	}
	w.used, w.tree = "walk-"+path.Base(packet.Jobs[i].Path), "git:"+packet.Tree
	if !slices.Contains(w.old.Receipts, w.used) {
		return w, fmt.Errorf("the requesting packet's walk %s is not of the old demand", w.used)
	}
	for _, d := range []demandIn{w.old, w.fresh} {
		for _, name := range d.Receipts {
			var r walkIn
			err := json.Unmarshal(l.file(name), &r)
			k := r.Result.Walk
			if err != nil || k.Plan != d.Plan || k.Planned != k.Observed || len(k.Disagreements) > 0 || "git:"+k.Walked.Tree != w.tree || k.Walked.Tree != r.Identity.Tree {
				return w, errors.Join(err, fmt.Errorf("%s is no full, clean walk of plan %s on the packet's tree", name, d.Plan))
			}
			w.walks[name] = r
		}
	}
	_, link, _ := strings.Cut(w.walks[w.fresh.Receipts[0]].Result.Walk.ReviewEvent, "-")
	if link == "" || !strings.HasPrefix(packet.InputDigest, link) {
		return w, fmt.Errorf("the new walk's review_event %q does not link its requesting packet", link)
	}
	return w, nil
}

func walkDemandInputs(l legacy) error {
	_, err := readWalkDemand(l)
	return err
}

// legacyRunners run a golden's M0 steps, in order, through the real
// components.
var legacyRunners = map[string]func(*testing.T, legacy) ([]Step, []string, error){
	"f16-walk-demand":     walkDemandSteps,
	"f16-inline-fallback": inlineFallbackSteps,
}

// demandKey is the golden's demand_key_assumption, an open point of the
// README: demand.key = H(plan_sha256, subject), without the requesting
// review. The grant is not part of it, nor of reuse equivalence (13), so
// the two old walks, under different grants, are one demand.
func demandKey(plan, tree string) types.Digest {
	sum := sha256.Sum256([]byte(plan + "\x00" + tree))
	return types.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// walkDemandSteps: the review the requesting packet records used an old
// plan's walk. Offered for the new demand, that receipt is inapplicable,
// and the review waits structurally (step 1, internal/receipt through
// graph.Resolve and the evaluator). Step 2's M0 part: once the old demand
// has spent its attempts, the new demand's walk is not cap-exhausted
// (graph.Exhausted); its evidence_observed result is M1's. Against the new
// plan's walk, G5's equivalence re-runs the review (step 3); against the
// other walk of the old plan it does not (step 4). A walk is a receipt with
// one clean predicate per observed step, about the walked tree under its
// demand.
func walkDemandSteps(t *testing.T, l legacy) ([]Step, []string, error) {
	w, err := readWalkDemand(l)
	if err != nil {
		return nil, nil, err
	}
	g, closeFn, err := openGraph(t.Context(), t.TempDir())
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = closeFn() }()
	oldKey, newKey := demandKey(w.old.Plan, w.tree), demandKey(w.fresh.Plan, w.tree)
	receipt := func(name string, demand types.Digest) (types.Ref, json.RawMessage, error) {
		var ps []types.Predicate
		for s := range w.walks[name].Result.Walk.Observed {
			ps = append(ps, types.Predicate{Name: fmt.Sprintf("step-%02d", s+1), Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Evidence: []types.PredicateEvidenceItem{}})
		}
		return g.gateReceiptWith(w.tree, demand, nil, []types.Restriction{}, ps...)
	}
	ref, c, err := receipt(w.used, oldKey)
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
	evaluate := func(demand types.Digest) error {
		return g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Evaluate(tx, []types.NodeID{review}, g.env(w.tree, demand))
		})
	}
	if err == nil {
		var st Step
		st, err = g.resolved(review, w.tree, newKey, receiptsAt)
		o.steps = append(o.steps, st)
		err = errors.Join(err, evaluate(newKey))
	}
	if err == nil {
		st, _, serr := g.node(review)
		err = serr
		// P22a-6b (Main's decision): a refused receipt is a wait shown with
		// its reason, awaiting_receipt on receipt:<input>:<reason>, no
		// longer a structural wait with none.
		o.check(st.State == types.NodeStateStateHeld && len(st.Holds) == 1 && st.Holds[0].Hold == vocab.HoldAwaitingReceipt &&
			st.Holds[0].SignalInstance == "receipt:gate:evidence_inapplicable" && st.Result == nil,
			"the review is not held awaiting its receipt: %s, holds %v", st.State, st.Holds)
	}
	if err == nil {
		err = walkAttempts(g, &o, w, oldKey, newKey)
	}
	if err == nil {
		err = evaluate(oldKey)
	}
	if err == nil {
		var st Step
		st, err = g.commit(review, types.ResultKindReview, g.env(w.tree, oldKey))
		o.check(st == cleanStep, "the review on the old walk did not commit: %s", st)
	}
	other := w.old.Receipts[1-slices.Index(w.old.Receipts, w.used)]
	for _, next := range []struct {
		name   string
		demand types.Digest
	}{{w.fresh.Receipts[0], newKey}, {other, oldKey}} {
		var now types.ResultID
		same := false
		if err == nil {
			ref, c, err = receipt(next.name, next.demand)
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

// walkAttempts is step 2's M0 part: with as many attempts per demand as the
// old demand's walks, after those walks the old demand's evidence node is
// cap-exhausted and the new demand's is not (E2, counting per demand).
func walkAttempts(g *graphFix, o *observe, w walkDemand, oldKey, newKey types.Digest) error {
	per := len(w.old.Receipts)
	total := per + len(w.fresh.Receipts)
	tree, err := g.tree()
	var a, b types.NodeID
	if err == nil {
		a, err = g.walkOn(tree, oldKey, per, total)
	}
	for i := 0; err == nil && i < per; i++ {
		if i > 0 {
			err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Invalidate(tx, a) })
		}
		if err == nil {
			_, err = g.attempt(a)
		}
	}
	if err == nil {
		err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Invalidate(tx, a) })
	}
	if err == nil {
		b, err = g.walkOn(tree, newKey, per, total)
	}
	var spent, fresh bool
	if err == nil {
		err = g.s.View(g.ctx, func(tx *eventstore.Tx) (err error) {
			if spent, err = graph.Exhausted(tx, a); err == nil {
				fresh, err = graph.Exhausted(tx, b)
			}
			return err
		})
	}
	o.check(spent && !fresh, "step 2: the old demand exhausted %v, the new demand exhausted %v; want true, false", spent, fresh)
	return err
}

// inlineFallbackSteps: the primary review attempt, from the controller's
// facts in the capture (no accepted result, nothing changed, both setup
// preconditions) and the stream's credit event, classified as an adapter's
// class_hint would be, goes through G2's outcome table and the exit map
// (step 1). The result is could_not_run with the credit reason, not
// completed and with no value, never an empty review; sealing the
// attempt's AttemptRecord is credit-before-first-edit's. The review's
// params are a review's (subject and roster); its one reviewer is the
// primary, of the gpt family (fallback-primary-usage.json's modelUsage).
func inlineFallbackSteps(t *testing.T, l legacy) ([]Step, []string, error) {
	var in struct {
		Lease struct{ Role string } `json:"lease"`
	}
	evs, err := rollout(l.file("rollout-*.jsonl"))
	err = errors.Join(err, json.Unmarshal(l.golden.Inputs, &in))
	var exit exitmap.Exit
	for _, ev := range evs {
		if ev.noCredits() {
			exit.Hints = append(exit.Hints, "credit_exhausted")
		}
	}
	nid, ierr := ids.New("nod")
	params := json.RawMessage(`{"subject":"candidate","roster":[{"executor":"exe_01m3250v9xft08ssr4ygq6g2np","family":"gpt"}]}`)
	if err = errors.Join(err, ierr); err != nil {
		return nil, nil, err
	}
	n := types.Node{ID: types.NodeID(nid), Kind: types.NodeKindReview, DefinitionVersion: "1", Round: 1, Params: params, Inputs: []types.InputSpec{},
		Requires: types.NodeRequires{Role: ptr(types.Name(in.Lease.Role)), Capability: ptr(types.Name("review")), Resources: []types.ResourceGroupKind{types.ResourceGroupKindAccountQuota}}}
	key, err := graph.Key(n, map[string]graph.Resolved{}, 1)
	var r types.Result
	if err == nil {
		r, err = graph.Build(key, graph.BuildFacts{Project: gProject, Submitted: false, Exit: exit})
	}
	var o observe
	o.steps = append(o.steps, Step{vocab.Outcome(r.Outcome), r.Reason.String()})
	o.check(graph.Check(r) == nil, "not a valid records/result: %v", graph.Check(r))
	o.check(!r.Completed && len(r.Value) == 0, "the attempt's result reads as finished work: %+v", r)
	return o.steps, o.violations, err
}

// legacyNames are the goldens; any other entry is a regular file.
func legacyNames(t *testing.T) []string {
	entries, err := os.ReadDir(legacyRoot)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		switch {
		case e.IsDir():
			names = append(names, e.Name())
		case !e.Type().IsRegular():
			t.Fatalf("fixtures/legacy/%s is neither a golden nor a regular file", e.Name())
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
	rep := func(old, new string) func(string) string {
		return func(s string) string { return strings.Replace(s, old, new, 1) }
	}
	cases := map[string]struct {
		golden, file, want string
		edit               func(string) string
	}{
		"a trimmed terminal record": {"f16-inline-fallback", "rollout-*.jsonl", "task_complete", func(s string) string {
			return s[:strings.LastIndex(strings.TrimSuffix(s, "\n"), "\n")+1]
		}},
		"a trimmed credit event": {"f16-inline-fallback", "rollout-*.jsonl", "credit event", func(s string) string {
			return regexp.MustCompile(`(?m)^.*token_count.*\n`).ReplaceAllString(s, "")
		}},
		"a primary that completed": {"f16-inline-fallback", "fallback-primary-usage.json", "completed or did work", rep(`"completed": false`, `"completed": true`)},
		"a missing walk field":     {"f16-walk-demand", "walk-1790945582297587810.json", "result.walk.plan_sha256", drop("plan_sha256")},
		"a null walk field":        {"f16-walk-demand", "walk-1790902623505136290.json", "disagreements", rep(`"disagreements": [],`, `"disagreements": null,`)},
		"a missing golden field":   {"closure-record-finding-r4", "golden.json", "required field", drop("class")},
		"an unknown golden field":  {"edge-owned-errors", "golden.json", "unknown field", rep("{", `{"extra": 1,`)},
		"variants of another type": {"edge-owned-errors", "golden.json", "variants", func(s string) string {
			var d map[string]any
			if json.Unmarshal([]byte(s), &d) != nil {
				return s
			}
			d["variants"] = 42
			b, _ := json.Marshal(d)
			return string(b)
		}},
		"an unaccounted error": {"edge-owned-errors", "edge-*.log", "does not account", rep("ERROR tests/", "")},
		"a consistently trimmed log": {"edge-owned-errors", "edge-*.log", "the incident 6 ERROR", func(s string) string {
			return strings.Replace(regexp.MustCompile(`(?m)^ERROR tests/test_release_proof.py::test_rollback_refuses_without_its_backup\n`).ReplaceAllString(s, ""), "6 errors in", "5 errors in", 1)
		}},
		"a trimmed finding": {"closure-record-finding-r4", "closure-record-finding-r4.json", "expected steps", func(s string) string {
			var d map[string]any
			if json.Unmarshal([]byte(s), &d) != nil {
				return s
			}
			d["findings"] = d["findings"].([]any)[1:]
			b, _ := json.Marshal(d)
			return string(b)
		}},
		"a blank finding field":     {"closure-record-finding-r4", "closure-record-finding-r4.json", "severity", rep(`"severity": "P2"`, `"severity": ""`)},
		"a digest not anonymised":   {"f16-inline-fallback", "session.json", "not a synthetic digest", rep(`"5eed0003`, `"abcd0003`)},
		"a 5eed digest, no counter": {"f16-inline-fallback", "session.json", "not a synthetic digest", rep(`"5eed000300000000`, `"5eedc0ffee9a8b7c`)},
		"a short raw digest":        {"f16-walk-demand", "walk-1790902623505136290.json", "not a synthetic digest", rep(`"reason": "",`, `"reason": "deadbeefcafe1234",`)},
		"a real path":               {"f16-walk-demand", "walk-1790902623505136290.json", "/home/mega", rep(`/home/user/runtime`, `/home/mega/runtime`)},
		"a real host":               {"f16-walk-demand", "walk-1790902623505136290.json", "http://lab.example", rep(`http://host-a:8300`, `http://lab.example:8300`)},
		"a source that is not here": {"f16-walk-demand", "golden.json", "names neither", rep("t/fixtures/f16-walk-demand/", "t/fixtures/other/")},
		"one plan for both demands": {"f16-walk-demand", "golden.json", "two plans", rep(`"plan_sha256": "5eed0028`, `"plan_sha256": "5eed001b`)},
		"another requesting packet": {"f16-walk-demand", "golden.json", "not of the old demand", rep(`packet-1524-5eed002400000000.json`, `packet-1538-5eed002a00000000.json`)},
		"an unlinked review_event":  {"f16-walk-demand", "walk-1790945582297587810.json", "does not link", rep(`-5eed002400000000"`, `-5eed00ab00000000"`)},
		"a partial walk":            {"f16-walk-demand", "walk-1790945582297587810.json", "no full, clean walk", rep(`"observed": 24`, `"observed": 18`)},
		"a walk of another tree":    {"f16-walk-demand", "walk-1790902623505136290.json", "no full, clean walk", func(s string) string { return strings.Replace(s, `"tree": "5eed0005`, `"tree": "5eed0099`, 2) }},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			root, edited := copyGolden(t, c.golden, c.file, c.edit)
			_, err := loadLegacy(root, c.golden)
			if !edited || err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("edited %v; loaded with %v, want a refusal naming %q", edited, err, c.want)
			}
		})
	}
	t.Run("an entry that is not a regular file", func(t *testing.T) {
		root, _ := copyGolden(t, "edge-owned-errors", "", nil)
		if err := os.Symlink("golden.json", filepath.Join(root, "edge-owned-errors", "link.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := loadLegacy(root, "edge-owned-errors"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("loaded with %v", err)
		}
	})
}

// copyGolden copies golden into a temporary root, editing the files that
// match file; edited reports whether an edit changed one.
func copyGolden(t *testing.T, golden, file string, edit func(string) string) (root string, edited bool) {
	root = t.TempDir()
	src := filepath.Join(legacyRoot, golden)
	entries, err := os.ReadDir(src)
	if err != nil || os.Mkdir(filepath.Join(root, golden), 0o700) != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if ok, _ := path.Match(file, e.Name()); ok && edit != nil {
			after := edit(string(b))
			edited, b = edited || after != string(b), []byte(after)
		}
		if err := os.WriteFile(filepath.Join(root, golden, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, edited
}
