// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session/inproc"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/wire"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// httpChatTools is the tool surface adapters/http-chat describes, whatever
// its role (adapter.ts: the edit loop's).
const httpChatTools = `"read_file","list_dir","write_file","delete_file"`

// Decisions a reviewer peer submits.
const (
	goDecision   = `{"decision":"go","summary":"meets the task","findings":[]}`
	noGoDecision = `{"decision":"no_go","summary":"a defect","findings":[{"severity":"high","title":"` + hiddenTest + ` fails: wrong refund","detail":"refund(-5) is accepted"}]}`
	notADecision = `{"decision":"maybe","summary":"","findings":[]}`
)

// reviewerSeen is what a reviewer peer was prepared with: its params, its
// prompt, and its workspace's entries then.
type reviewerSeen struct {
	prep    types.AdapterPrepareParams
	prompt  []byte
	entries int
}

// reviewer is a reviewer adapter in this process: as builder, but on run
// it writes decision to its exchange's out/ and submits it (kind decision),
// as adapters/http-chat's reviewer does; it sends what it was prepared with
// on seen. It describes http-chat's tool surface (httpChatTools: it learns
// its role only at prepare); with requestTool, it requests read_file on run
// (a tool.request) before it submits.
func reviewer(decision string, requestTool bool, seen chan<- reviewerSeen) *inproc.Peer {
	return inproc.New(func(in io.Reader, out io.Writer) {
		r, w := wire.NewReader(in), wire.NewWriter(out)
		var prep types.AdapterPrepareParams
		for {
			line, err := r.Next()
			var req wire.Request
			if err != nil || json.Unmarshal(line, &req) != nil {
				return
			}
			res := json.RawMessage(builderClean)
			switch req.Method {
			case "describe":
				d := strings.Replace(builderDescribe, `"capabilities":["patch"]`, `"capabilities":["decision"]`, 1)
				res = json.RawMessage(strings.Replace(d, `"tool_surface":[]`, `"tool_surface":[`+httpChatTools+`]`, 1))
			case "prepare":
				if json.Unmarshal(req.Params, &prep) != nil {
					return
				}
				if seen != nil {
					b, _ := os.ReadFile(filepath.Join(prep.Exchange.In, adapterhost.PromptName))
					entries, _ := os.ReadDir(prep.Workspace.Path)
					seen <- reviewerSeen{prep, b, len(entries)}
				}
			case "run":
				if os.WriteFile(filepath.Join(prep.Exchange.Out, "decision.json"), []byte(decision), 0o600) != nil {
					return
				}
				var params types.AdapterRunParams
				if json.Unmarshal(req.Params, &params) != nil {
					return
				}
				if requestTool {
					ev, err := wire.Encode(map[string]any{"jsonrpc": wire.Version, "method": "adapter.event", "params": map[string]any{"event": "tool.request",
						"attempt": params.Attempt, "fencing_token": params.FencingToken, "call": params.Call, "turn": 1, "tool": "read_file", "request": params.Prompt}})
					if err != nil || w.Write(ev) != nil {
						return
					}
				}
				sum := sha256.Sum256([]byte(decision))
				run := types.AdapterRunResult{Outcome: types.OutcomeFound, Reason: vocab.ReasonCompletedWithFindings, Detail: "the reviewer's decision",
					Result: &types.AdapterRunResultResultValue{Kind: types.AdapterRunResultResultValueKindDecision,
						Handle: types.AdapterHandle{Attempt: prep.Attempt, Handle: "decision.json", Digest: types.Digest("sha256:" + hex.EncodeToString(sum[:])), Size: types.Amount(len(decision))}}}
				if res, err = json.Marshal(run); err != nil {
					return
				}
			}
			b, err := wire.Encode(map[string]any{"jsonrpc": wire.Version, "id": req.ID, "result": res})
			if err != nil || w.Write(b) != nil {
				return
			}
		}
	})
}

// reviewRun is a red gate's review (the candidate fails hiddenTest) run by
// a reviewer peer submitting decision, until the review leaves claimed (at
// most 12 passes): the world and what the peer was prepared with.
func reviewRun(t *testing.T, decision string) (*runWorld, reviewerSeen) {
	t.Helper()
	rw := gateWorld(t, goEvents("TestA=pass"), map[string]string{"gate-events.json": goEvents("TestA=pass", hiddenTest+"=fail")}, nil)
	if v := rw.gating(t); v.State.State != types.NodeStateStateCompleted {
		t.Fatalf("the gate: %s, holds %+v", v.State.State, v.State.Holds)
	}
	seen := make(chan reviewerSeen, 4)
	rw.reviewRunning(stubRegistry(t, nil, nil), reviewer(decision, false, seen))
	for range 12 {
		rw.passesFor(t, 1, 0)
		if v := reviewNodes(rw)[types.NodeKindReview]; v.State.State != types.NodeStateStateClaimed && v.State.State != types.NodeStateStateWaiting && v.State.State != types.NodeStateStateRunnable {
			break
		}
	}
	select {
	case s := <-seen:
		return rw, s
	default:
		t.Fatal("no reviewer was prepared")
	}
	return rw, reviewerSeen{}
}

// reviewResult is the review's results, from the log, in record order.
func reviewResults(rw *runWorld, review types.NodeID) []types.ResultRecorded {
	var out []types.ResultRecorded
	rw.w.view(func(tx *eventstore.Tx) error {
		keys, err := tx.Keys(graph.ResultProjection, "")
		for _, k := range keys {
			raw, _, gerr := tx.Get(graph.ResultProjection, k)
			var rr types.ResultRecorded
			if gerr == nil && json.Unmarshal(raw, &rr) == nil && rr.Node == review {
				out = append(out, rr)
			}
		}
		return err
	})
	return out
}

// P22a-6c: a placed review is run by adcd's runner as a reviewer: prepared
// with the role reviewer, no tool, a read-only empty workspace and the
// qualified limits, its prompt the reviewer intent's (review.Prompt: the
// gate's counts, never its ids; the brief's task and the candidate's
// diff; nothing of the acceptance, which the I18 walk refuses: 6b); a
// go decision is the review's empty/completed_clean result, by its
// attempt, and completes it.
func TestAGoReviewCompletesTheReview(t *testing.T) {
	rw, seen := reviewRun(t, goDecision)
	review := reviewNodes(rw)[types.NodeKindReview]
	if review.State.State != types.NodeStateStateCompleted || review.Outcome == nil || *review.Outcome != types.OutcomeEmpty {
		t.Fatalf("the review: %s %v, holds %+v", review.State.State, review.Outcome, review.State.Holds)
	}
	rs := reviewResults(rw, review.Node.ID)
	if len(rs) != 1 || rs[0].Attempt == nil || rs[0].Result.Reason != vocab.ReasonCompletedClean {
		t.Fatalf("its results: %+v", rs)
	}
	p := seen.prep
	if p.Role != "reviewer" || p.Workspace.Writable || len(p.ToolPolicy.Allowed) != 0 || p.Limits.MaxTurns != 1 || p.Limits.MaxOutputTokens != 8000 {
		t.Errorf("the reviewer's prepare: %+v", p)
	}
	if seen.entries != 0 {
		t.Errorf("the reviewer's workspace held %d entries", seen.entries)
	}
	_, gateSection, _ := strings.Cut(string(seen.prompt), "## Gate")
	gateSection, _, _ = strings.Cut(gateSection, "## Candidate change")
	if !strings.Contains(gateSection, "(1 passed, 1 failed, 0 skipped)") || strings.Contains(gateSection, hiddenTest) {
		t.Errorf("the prompt's gate: %q", gateSection)
	}
	if !strings.Contains(string(seen.prompt), "## Task\n\n# ") || !strings.Contains(string(seen.prompt), "gate-events.json") {
		t.Errorf("the prompt is not the brief's task and the candidate's diff: %q", seen.prompt)
	}
}

// P22a-6c: a no_go decision is found/completed_with_findings: its report
// published (the result's last ref, readable) and the builder-safe
// findings view stored, the failing gate test's id redacted from it.
func TestANoGoReviewPublishesItsReport(t *testing.T) {
	rw, _ := reviewRun(t, noGoDecision)
	review := reviewNodes(rw)[types.NodeKindReview]
	rs := reviewResults(rw, review.Node.ID)
	if review.State.State != types.NodeStateStateCompleted || len(rs) != 1 || rs[0].Result.Outcome != types.OutcomeFound ||
		rs[0].Result.Reason != vocab.ReasonCompletedWithFindings || len(rs[0].Result.Value) != 3 {
		t.Fatalf("the review: %s, results %+v", review.State.State, rs)
	}
	for _, ref := range rs[0].Result.Value {
		rc, err := rw.r.blobs.Open(string(ref.ContentDigest))
		if err != nil {
			t.Fatalf("%s %s: %v", ref.Type, ref.ID, err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if ref.Type == types.RefTypeView && strings.Contains(string(b), hiddenTest) {
			t.Errorf("the findings view holds the gate's id: %s", b)
		}
	}
	if rs[0].Result.Value[1].Type != types.RefTypeView {
		t.Errorf("no findings view: %+v", rs[0].Result.Value)
	}
}

// P22a-6c: a decision that is no typed decision is could_not_run/
// degenerate_output; nothing is selected, and the review is matched again
// (its lease revoked, a new one granted).
func TestADegenerateDecisionIsNotSelected(t *testing.T) {
	rw, _ := reviewRun(t, notADecision)
	review := reviewNodes(rw)[types.NodeKindReview]
	rs := reviewResults(rw, review.Node.ID)
	if len(rs) == 0 || rs[0].Result.Outcome != types.OutcomeCouldNotRun || rs[0].Result.Reason != vocab.ReasonDegenerateOutput || review.State.Result != nil {
		t.Fatalf("the review: %s, results %+v", review.State.State, rs)
	}
}

// P22a-6c: the commit checks the reviewer's independence (P16b's commit
// half): with the runner's registry now giving the build's executor the
// reviewer's family, the review's go is recorded as
// could_not_run/reviewer_not_independent, nothing is selected, and the
// review is held reviewer_unavailable, run once.
func TestAReviewerOfTheBuildersFamilyIsRefusedAtCommit(t *testing.T) {
	rw := gateWorld(t, goEvents("TestA=pass"), map[string]string{"README.md": "hello\n"}, nil)
	if v := rw.gating(t); v.State.State != types.NodeStateStateCompleted {
		t.Fatalf("the gate: %s", v.State.State)
	}
	rw.reviewRunning(stubRegistry(t, nil, nil), reviewer(goDecision, false, nil))
	rw.r.env.Families = builderFamily{stubRegistry(t, nil, nil)} // at commit only
	var review graph.NodeView
	var rs []types.ResultRecorded
	for range 8 { // to the pass that runs it: the loop's registry, unlike the runner's, then clears the hold (Unheld)
		rw.passesFor(t, 1, 0)
		if review = reviewNodes(rw)[types.NodeKindReview]; len(reviewResults(rw, review.Node.ID)) > 0 {
			break
		}
	}
	rs = reviewResults(rw, review.Node.ID)
	if len(rs) != 1 || rs[0].Attempt == nil || rs[0].Result.Outcome != types.OutcomeCouldNotRun || rs[0].Result.Reason != vocab.ReasonReviewerNotIndependent {
		t.Fatalf("its results: %+v", rs)
	}
	if review.State.State != types.NodeStateStateHeld || review.State.Result != nil || len(review.State.Holds) != 1 ||
		review.State.Holds[0].Hold != vocab.HoldReviewerUnavailable || !strings.HasPrefix(review.State.Holds[0].SignalInstance, "contributor:") {
		t.Errorf("the review: %s, holds %+v", review.State.State, review.State.Holds)
	}
}

// builderFamily gives the stub builder the stub reviewer's family: the
// candidate's contributor is of the roster's family.
type builderFamily struct{ graph.Families }

func (f builderFamily) Family(e types.ExecutorID) string {
	if e == stubBuilder {
		return f.Families.Family(stubReviewer)
	}
	return f.Families.Family(e)
}

// Opus 22a-6b M2: a candidate that gives no review context (here a diff
// over review.MaxDiff) is the review's verdict, never the run's failure:
// failed/unsafe_output, the bound named, recorded with no attempt, the
// lease ended unlaunched, and no reviewer prepared.
func TestAnUnreviewableCandidateIsTheReviewsVerdict(t *testing.T) {
	big := strings.Repeat("a long line of the candidate's own text\n", 16<<10)
	rw := gateWorld(t, goEvents("TestA=pass"), map[string]string{"big.txt": big}, nil)
	if v := rw.gating(t); v.State.State != types.NodeStateStateCompleted {
		t.Fatalf("the gate: %s, holds %+v", v.State.State, v.State.Holds)
	}
	seen := make(chan reviewerSeen, 4)
	rw.reviewRunning(stubRegistry(t, nil, nil), reviewer(goDecision, false, seen))
	rw.passesFor(t, 8, 0)
	review := roundOf(t, rw, 1).review // round 1's: its verdict repairs the build (P22a-6d)
	rs := reviewResults(rw, review.Node.ID)
	if review.State.State != types.NodeStateStateCompleted || len(rs) != 1 || rs[0].Attempt != nil || rs[0].Result.Outcome != types.OutcomeFailed ||
		rs[0].Result.Reason != vocab.ReasonUnsafeOutput || !strings.Contains(rs[0].Result.Detail, "over 524288 bytes") {
		t.Fatalf("the review: %s, results %+v", review.State.State, rs)
	}
	if _, live := leaseOn(rw.w, review.Node.ID); live {
		t.Error("its lease is live")
	}
	if len(seen) != 0 {
		t.Error("a reviewer was prepared")
	}
}

// Opus 22a-6c H1: the production reviewer adapter describes http-chat's
// tool surface (the edit loop's) whatever its role, so the host allows it
// (Spec.Tools), as qualify's reviewer does: it is prepared, with no tool
// allowed, and its go completes the review. (Every row's reviewer peer
// describes that surface.)
func TestAReviewerDescribingHTTPChatsToolsIsPrepared(t *testing.T) {
	rw, seen := reviewRun(t, goDecision)
	review := reviewNodes(rw)[types.NodeKindReview]
	if review.State.State != types.NodeStateStateCompleted || len(seen.prep.ToolPolicy.Allowed) != 0 {
		t.Fatalf("the review: %s, allowed %v", review.State.State, seen.prep.ToolPolicy.Allowed)
	}
}

// Opus 22a-6c H1: a reviewer that requests a tool during its run is
// refused (the host serves no tool: unserve): its decision is never taken,
// and the review is not completed.
func TestAReviewerRequestingAToolIsRefused(t *testing.T) {
	rw := gateCompleted(t)
	seen := make(chan reviewerSeen, 8)
	rw.reviewRunning(stubRegistry(t, nil, nil), reviewer(goDecision, true, seen))
	rw.passesFor(t, 8, 0)
	review := reviewNodes(rw)[types.NodeKindReview]
	rs := reviewResults(rw, review.Node.ID)
	if len(seen) == 0 || review.State.State == types.NodeStateStateCompleted || len(rs) == 0 {
		t.Fatalf("the review: %s, prepared %d, results %+v", review.State.State, len(seen), rs)
	}
	for _, rr := range rs {
		if rr.Result.Outcome != types.OutcomeCouldNotRun || rr.Result.Kind != types.ResultKindReview {
			t.Errorf("a result: %+v", rr.Result)
		}
	}
}

// Opus 22a-6c L2: an unreviewable verdict whose commit is refused for
// independence records nothing and holds the review reviewer_unavailable
// (graph.HoldNotIndependent), its lease ended unlaunched.
func TestAnUnreviewableVerdictOfADependentReviewerIsHeld(t *testing.T) {
	big := strings.Repeat("a long line of the candidate's own text\n", 16<<10)
	rw := gateWorld(t, goEvents("TestA=pass"), map[string]string{"big.txt": big}, nil)
	if v := rw.gating(t); v.State.State != types.NodeStateStateCompleted {
		t.Fatalf("the gate: %s", v.State.State)
	}
	rw.reviewRunning(stubRegistry(t, nil, nil), reviewer(goDecision, false, nil))
	rw.r.env.Families = builderFamily{stubRegistry(t, nil, nil)} // at commit only
	var review graph.NodeView
	for range 8 {
		rw.passesFor(t, 1, 0)
		if review = reviewNodes(rw)[types.NodeKindReview]; review.State.State == types.NodeStateStateHeld {
			break
		}
	}
	if rs := reviewResults(rw, review.Node.ID); len(rs) != 0 || review.State.State != types.NodeStateStateHeld ||
		review.State.Holds[0].Hold != vocab.HoldReviewerUnavailable {
		t.Fatalf("the review: %s, holds %+v, results %+v", review.State.State, review.State.Holds, rs)
	}
	if _, live := leaseOn(rw.w, review.Node.ID); live {
		t.Error("its lease is live")
	}
}

// P22a-6c: a review attempt whose session's end is unverified is
// abandoned as a build's is: could_not_run/ambiguous_state, a review's
// result, its decision never taken.
func TestAnUnverifiedReviewIsAbandoned(t *testing.T) {
	rw := gateCompleted(t)
	runPrepared = func(p *adapterhost.Prepared, ctx context.Context) (adapterhost.Outcome, error) {
		out, err := p.Run(ctx)
		out.Session = session.ErrUnverified
		return out, err
	}
	t.Cleanup(func() { runPrepared = (*adapterhost.Prepared).Run })
	rw.reviewRunning(stubRegistry(t, nil, nil), reviewer(goDecision, false, nil))
	rw.passesFor(t, 8, 0)
	review := reviewNodes(rw)[types.NodeKindReview]
	rs := reviewResults(rw, review.Node.ID)
	if len(rs) == 0 || rs[0].Result.Kind != types.ResultKindReview || rs[0].Result.Outcome != types.OutcomeCouldNotRun ||
		rs[0].Result.Reason != vocab.ReasonAmbiguousState || review.State.State == types.NodeStateStateCompleted {
		t.Fatalf("the review: %s, results %+v", review.State.State, rs)
	}
}

// #403 with P22a-6c: a reviewer's run past its disk bound (sandbox.ErrLeft,
// measured) is the reviewer's fault, never the candidate's: the review's
// attempt is abandoned (could_not_run/ambiguous_state, a review's), never
// the unreviewable verdict, and the review is matched again.
func TestAReviewPastItsDiskBoundIsAbandoned(t *testing.T) {
	rw := gateCompleted(t)
	runPrepared = func(p *adapterhost.Prepared, ctx context.Context) (adapterhost.Outcome, error) {
		out, err := p.Run(ctx)
		out.Checked = sandbox.ErrLeft
		return out, err
	}
	t.Cleanup(func() { runPrepared = (*adapterhost.Prepared).Run })
	rw.reviewRunning(stubRegistry(t, nil, nil), reviewer(goDecision, false, nil))
	rw.passesFor(t, 8, 0)
	review := reviewNodes(rw)[types.NodeKindReview]
	rs := reviewResults(rw, review.Node.ID)
	if len(rs) == 0 || review.State.State == types.NodeStateStateCompleted {
		t.Fatalf("the review: %s, results %+v", review.State.State, rs)
	}
	for _, rr := range rs {
		if rr.Result.Kind != types.ResultKindReview || rr.Result.Outcome != types.OutcomeCouldNotRun || rr.Result.Reason != vocab.ReasonAmbiguousState {
			t.Errorf("a result: %+v", rr.Result)
		}
	}
}
