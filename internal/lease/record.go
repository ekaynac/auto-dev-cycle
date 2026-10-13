// SPDX-License-Identifier: Apache-2.0

package lease

// M1's minimal AttemptRecord (docs/design/m1/00-slice-plan.md §6 L1; 05
// "AttemptRecord").
//
// Decisions:
//   - M1 seals a minimal record: the fields the projector's checkpoint
//     check binds (the attempt, its token, base tree and contract revision,
//     and its unit's project and task), and the partial diff the controller
//     computed from the attempt's workspace, null when it changed nothing.
//   - Nothing continues from it in M1, so it is not pinned, and it holds no
//     failing tests, no builder notes, and the published empty list as its
//     trail and evidence manifest. Sealing at every builder exit, and
//     continuation, stay in M2.
//   - The seal's reason is derived from the attempt, never chosen
//     (SealReason). repair_handoff is M2's.
//   - The record and its blobs are published before the event that names
//     them (03): a refused seal leaves only blobs nothing references.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// Publisher publishes a blob and returns its digest (blobstore.Store).
type Publisher interface {
	Put(r io.Reader, declared string) (string, error)
}

// SealReason is the seal reason of a, an attempt that stopped publishing:
// finishing is completed; stopped for a drain, drain; lost on a controller
// restart, host_restart; any other stop or loss, abrupt_loss.
func SealReason(a types.Attempt) (types.AttemptRecordSealReason, error) {
	switch {
	case a.Status == types.AttemptStatusFinishing:
		return types.AttemptRecordSealReasonCompleted, nil
	case a.Status == types.AttemptStatusStopping && a.StopReason != nil && *a.StopReason == types.AttemptStopReasonValueDrain:
		return types.AttemptRecordSealReasonDrain, nil
	case a.Status == types.AttemptStatusLost && a.LostReason != nil && *a.LostReason == types.AttemptLostReasonValueControllerRestart:
		return types.AttemptRecordSealReasonHostRestart, nil
	case a.Status == types.AttemptStatusStopping || a.Status == types.AttemptStatusLost:
		return types.AttemptRecordSealReasonAbruptLoss, nil
	}
	return "", fmt.Errorf("%w: a %s attempt is not sealed", ErrTransition, a.Status)
}

// SealMinimal proposes attempt.sealed for attempt id, at the expected
// revision, with M1's minimal AttemptRecord sealed by actor by: diff is the
// controller-side diff's published blob, nil when the attempt changed
// nothing; units gives its unit's project and task.
func SealMinimal(tx *eventstore.Tx, id types.AttemptID, expected int64, units Units, blobs Publisher, diff *types.Digest, by types.ActorID) ([]eventstore.Event, error) {
	if tx.Now().IsZero() || units == nil {
		return nil, errors.New("lease: a seal runs in a command's transaction, with the unit binding")
	}
	a, err := current[types.Attempt](tx, AttemptProjection, string(id), expected)
	if err != nil {
		return nil, err
	}
	reason, err := SealReason(a)
	if err != nil {
		return nil, err
	}
	project, task, ok, err := units.ProjectTask(tx, a.Unit)
	if err != nil || !ok {
		return nil, errors.Join(err, fmt.Errorf("lease: unit %s has no project and task to bind the record to", a.Unit))
	}
	empty, err := blobs.Put(bytes.NewReader([]byte("[]")), "")
	if err != nil {
		return nil, err
	}
	b, err := contract.Encode("records/attempt-record", types.AttemptRecord{Attempt: a.ID, Project: project, Task: task,
		ContractRevision: a.Base.ContractRevision, FencingToken: a.FencingToken, BaseTree: a.Base.Tree, PartialDiff: diff,
		FailingTests: []string{}, Trail: types.Digest(empty), EvidenceManifest: types.Digest(empty),
		Seal: types.AttemptRecordSeal{Reason: reason, At: types.Time(tx.Now().UTC().Format(time.RFC3339Nano)), SealedBy: by}})
	if err != nil {
		return nil, err
	}
	record, err := blobs.Put(bytes.NewReader(b), "")
	if err != nil {
		return nil, err
	}
	return ChangeAttempt(tx, id, expected, AttemptEvent{Kind: Sealed, Checkpoint: types.Digest(record)})
}
