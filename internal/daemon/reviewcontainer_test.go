// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// runnerHold is node view v's runner hold signal, if any.
func runnerHold(holds []types.NodeHold) string {
	for _, h := range holds {
		if isRunnerHold(h.SignalInstance) {
			return h.SignalInstance
		}
	}
	return ""
}

// Mythos chunk 4a H1, through the runner and adcd's container adapter (not
// peerStarter): under a profile listing a lockfile (py-mini's
// requirements-dev.txt), a review's empty workspace is no base to hash, so
// its adapter is prepared (Ready is reached, the workspace read-only), and
// it is never held on the profile. A build whose base lacks the lockfile is
// still held on the profile, nothing prepared.
func TestAReviewUnderALockfileProfileIsPrepared(t *testing.T) {
	lockfiles := map[string]string{"requirements-dev.txt": "sha256:" + strings.Repeat("c", 64)}
	rw := gateCompleted(t)
	rw.reviewRunning(stubRegistry(t, nil, nil), reviewer(goDecision, false, nil))
	box, want := fakeContainer(t, "python", lockfiles)
	box.interact = sandbox.ErrSpec // the run stops at Ready: the host's, retried
	rw.r.adapters = &containerAdapter{Box: box, Want: want}
	for range 12 {
		if len(box.runs) > 0 {
			break
		}
		rw.passesFor(t, 1, 0)
	}
	review := reviewNodes(rw)[types.NodeKindReview]
	if len(box.runs) != 1 || !box.runs[0].ReadOnlyWorkspace || strings.HasPrefix(runnerHold(review.State.Holds), runnerText("profile ")) {
		t.Fatalf("the review: %s, holds %+v; interactive runs %+v", review.State.State, review.State.Holds, box.runs)
	}

	build := newRunWorld(t, true, builder(nil, false, nil))
	box, want = fakeContainer(t, "python", lockfiles)
	build.r.adapters = &containerAdapter{Box: box, Want: want}
	build.untilRun(t)
	if h := build.heldOn(); !strings.HasPrefix(h, runnerText("profile ")) || len(box.runs) != 0 {
		t.Errorf("a build whose base lacks its lockfile: held on %q, interactive runs %+v", h, box.runs)
	}
}
