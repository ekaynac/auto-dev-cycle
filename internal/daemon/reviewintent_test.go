// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/intent"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

// hiddenTest is the hostile-gate-output shape's id: a failing test whose
// name a reviewer's gate summary never carries.
const hiddenTest = "TestHiddenSecretKey"

// reviewLaunch is a red gate's review (the candidate fails hiddenTest),
// recorded, claimed and placed on the stub reviewer (22a-6a), and its
// reviewer's intent built (reviewerIntent) and recorded through the
// launch, edited by edit first (nil: as built), as 22a-6c's run will: the
// world, the intent and prompt built, the attempt, and the launch's error.
func reviewLaunch(t *testing.T, edit func(*types.ManifestIntent)) (*runWorld, types.ManifestIntent, []byte, types.AttemptID, error) {
	t.Helper()
	return reviewLaunchOn(t, map[string]string{"gate-events.json": goEvents("TestA=pass", hiddenTest+"=fail")}, edit)
}

// reviewLaunchOn is reviewLaunch over a build that writes files.
func reviewLaunchOn(t *testing.T, files map[string]string, edit func(*types.ManifestIntent)) (*runWorld, types.ManifestIntent, []byte, types.AttemptID, error) {
	t.Helper()
	rw := gateWorld(t, goEvents("TestA=pass"), files, nil)
	if v := rw.gating(t); v.State.State != types.NodeStateStateCompleted {
		t.Fatalf("the gate: %s %v, holds %+v", v.State.State, v.Outcome, v.State.Holds)
	}
	rw.reviewing(stubRegistry(t, nil, nil))
	rw.passesFor(t, 6, 0)
	review := reviewNodes(rw)[types.NodeKindReview]
	l, live := leaseOn(rw.w, review.Node.ID)
	if review.State.State != types.NodeStateStateClaimed || !live {
		t.Fatalf("the review: %s, its lease live %v", review.State.State, live)
	}
	pl, err := rw.r.reviewPlan(context.Background(), review.Node.ID, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	aid, cid, err := newRunIDs()
	if err != nil {
		t.Fatal(err)
	}
	mi, prompt, err := rw.r.reviewerIntent(context.Background(), pl, review.Node.ID, aid, cid)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		recordIntent = func(m types.ManifestIntent) (eventstore.Event, error) {
			m.Inputs = append([]types.Ref{}, m.Inputs...)
			m.Included = append([]types.ManifestIntentIncludedItem{}, m.Included...)
			m.Omitted = append([]types.ManifestIntentOmittedItem{}, m.Omitted...)
			edit(&m)
			return intent.Record(m)
		}
		t.Cleanup(func() { recordIntent = intent.Record })
	}
	build, _ := rw.attemptOf(t)
	miss, err := rw.r.place.launch(context.Background(), rw.r.store, rw.r.system, review.Node.ID, l.ID, aid, build.Base, &mi)
	if miss != nil {
		err = errors.Join(err, miss)
	}
	return rw, mi, prompt, aid, err
}

// P22a-6b (I18): the reviewer's intent, recorded at its launch and checked
// by its fold: stated as the reviewer's, its inputs the review's rendered
// context (the brief, the patch, the gate's receipt and report), the
// brief, the patch and the report included in that order at their blobs'
// sizes, the receipt omitted (policy), its rendered digest the prompt's;
// and the prompt renders the gate's verdict and counts, never its ids.
func TestTheReviewersIntentIsRecordedAtLaunch(t *testing.T) {
	rw, built, prompt, aid, err := reviewLaunch(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	mi, ok := recordedIntent(t, rw.w, aid)
	sum := sha256.Sum256(prompt)
	switch {
	case !ok || mi.ID != built.ID || mi.Consumer.Role != "reviewer" || knowledge.CheckIntent(mi) != nil:
		t.Fatalf("the recorded intent: %v %+v", ok, mi)
	case len(mi.Inputs) != 4 || len(mi.Included) != 3 || len(mi.Omitted) != 1 || mi.Omitted[0].Reason != types.ManifestIntentOmittedItemReasonPolicy ||
		mi.Omitted[0].Ref.Type != types.RefTypeReceipt || mi.Included[2].Ref.Type != types.RefTypeBlob:
		t.Fatalf("its inputs: %+v, included %+v, omitted %+v", mi.Inputs, mi.Included, mi.Omitted)
	case mi.RenderedDigest != types.Digest("sha256:"+hex.EncodeToString(sum[:])):
		t.Fatalf("its rendered digest %s is not the prompt's", mi.RenderedDigest)
	}
	_, gateSection, _ := strings.Cut(string(prompt), "## Gate")
	gateSection, _, _ = strings.Cut(gateSection, "## Candidate change")
	if !strings.Contains(gateSection, "red: not every test passed (1 passed, 1 failed, 0 skipped).") || strings.Contains(gateSection, hiddenTest) {
		t.Errorf("the prompt's gate: %q", gateSection)
	}
	if !strings.Contains(string(prompt), "## Task\n\n# ") {
		t.Errorf("the prompt's task is not the brief's: %q", prompt)
	}
}

// P22a-6b: the fold refuses a reviewer intent that is not what the launch
// builds, and the launch starts nothing: stated as a builder's (the
// builder's role on a review attempt), the receipt included (the raw gate
// result), the receipt omitted for another reason, the report omitted, an input of no node of the claim (the
// patch twice), another rendered digest or included size, the inputs in
// another order, and the report's ref under the brief's restriction (not
// its record's).
func TestAForgedReviewerIntentIsRefused(t *testing.T) {
	for name, edit := range map[string]func(*types.ManifestIntent){
		"stated as a builder's": func(m *types.ManifestIntent) { m.Consumer.Role = "builder" },
		"the receipt included": func(m *types.ManifestIntent) {
			m.Included = append(m.Included, types.ManifestIntentIncludedItem{Ref: m.Omitted[0].Ref, Order: 3, Bytes: 1, Purpose: "review input"})
			m.Omitted = []types.ManifestIntentOmittedItem{}
		},
		"the report omitted": func(m *types.ManifestIntent) {
			m.Omitted = append(m.Omitted, types.ManifestIntentOmittedItem{Ref: m.Included[2].Ref, Reason: types.ManifestIntentOmittedItemReasonPolicy})
			m.Included = m.Included[:2]
		},
		"the receipt omitted for budget": func(m *types.ManifestIntent) { m.Omitted[0].Reason = types.ManifestIntentOmittedItemReasonBudget },
		"another rendered digest":        func(m *types.ManifestIntent) { m.RenderedDigest = types.Digest("sha256:" + strings.Repeat("0", 64)) },
		"another included size":          func(m *types.ManifestIntent) { m.Included[1].Bytes++ },
		"the inputs reordered":           func(m *types.ManifestIntent) { m.Inputs[0], m.Inputs[1] = m.Inputs[1], m.Inputs[0] },
		"the brief for the patch": func(m *types.ManifestIntent) {
			m.Inputs[1], m.Included[1].Ref = m.Inputs[0], m.Inputs[0]
		},
		"the report under another restriction": func(m *types.ManifestIntent) {
			r := m.Included[2].Ref
			r.Restrictions = append([]types.Restriction{}, m.Inputs[0].Restrictions...)
			sealed, err := knowledge.Seal(r)
			if err != nil {
				t.Fatal(err)
			}
			m.Inputs[2], m.Included[2].Ref = sealed, sealed
		},
	} {
		t.Run(name, func(t *testing.T) {
			rw, _, _, aid, err := reviewLaunch(t, edit)
			if err == nil {
				t.Fatal("the forged intent was recorded")
			}
			if _, ok := recordedIntent(t, rw.w, aid); ok {
				t.Error("an intent was recorded")
			}
			rw.w.view(func(tx *eventstore.Tx) error {
				_, _, started, err := lease.AttemptAt(tx, aid)
				if started {
					t.Error("the attempt started")
				}
				return err
			})
		})
	}
}

// P22a-6b (I18 consumption): a review's context with an input that derives
// from the task's acceptance is refused before anything is rendered: the
// review's rendered context, with the acceptance's ref among its inputs,
// fails the reviewer's walk (intent.Reviewer).
func TestAReviewersContextFromTheAcceptanceIsRefused(t *testing.T) {
	rw, built, _, _, err := reviewLaunch(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	review := reviewNodes(rw)[types.NodeKindReview]
	rw.w.view(func(tx *eventstore.Tx) error {
		recs, err := intent.Load(tx, rw.r.blobs, rw.task)
		if err != nil {
			return err
		}
		acc := recs.Acceptance()
		if len(acc) == 0 {
			t.Fatal("the task holds no acceptance")
		}
		ref, err := knowledge.Seal(types.Ref{Type: types.RefTypeBlob, ID: string(acc[0]), Scope: built.Inputs[0].Scope, Lineage: []string{},
			Restrictions: built.Inputs[0].Restrictions, ContentDigest: acc[0]})
		if err != nil {
			return err
		}
		rendered, err := graph.Context(tx, review.Node.ID, rw.r.env)
		if err != nil {
			return err
		}
		rc, err := intent.Review(tx, rw.r.blobs, review.Node.ID, rendered)
		if err != nil {
			return err
		}
		ctx := rc.Rendered()
		ctx.Included = append(ctx.Included, ref)
		bytes := rc.Bytes
		bytes[ref.IdentityDigest] = 1
		if _, err := intent.Reviewer(intent.In{Call: built, Context: ctx, Bytes: bytes, Records: recs}); !errors.Is(err, intent.ErrAcceptanceInput) {
			t.Errorf("an acceptance input: %v", err)
		}
		return nil
	})
}

// Astra chunk 3 M2: an empty candidate (a build that changes nothing:
// patch/empty, no ref) is reviewed: its reviewer's intent lists the brief
// and the gate's report included and its receipt omitted, no patch, and
// the prompt says there is no change.
func TestAnEmptyCandidateIsReviewed(t *testing.T) {
	rw, _, prompt, aid, err := reviewLaunchOn(t, map[string]string{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mi, ok := recordedIntent(t, rw.w, aid)
	if !ok || len(mi.Inputs) != 3 || len(mi.Included) != 2 || len(mi.Omitted) != 1 || mi.Omitted[0].Ref.Type != types.RefTypeReceipt {
		t.Fatalf("the intent: %v, inputs %+v, included %+v", ok, mi.Inputs, mi.Included)
	}
	if !strings.Contains(string(prompt), "No changes: the candidate's diff against its base is empty.") || strings.Contains(string(prompt), "```") {
		t.Errorf("the prompt: %s", prompt)
	}
}
