// SPDX-License-Identifier: Apache-2.0

package lease

import "github.com/ekaynac/auto-dev-cycle/internal/eventstore"

// EventSchema is the payload schema of every lease and attempt event.
const EventSchema = "records/lease-event"

// OutputRefused records one adapter output the controller refused (09). It
// is not a transition: the attempt record stays as it is, so the projector
// ignores it, and an attempt with one is untrustworthy, derived at read time.
const (
	OutputRefused       = "attempt.output_refused/v1"
	OutputRefusalSchema = "records/attempt-output-refusal"
)

// transitions are the event names of note 05's tables, lease and attempt.
var transitions = []string{
	Requested, Waiting, Granted, StartRefused, Expired, Completed, Revoked, Preempted, Settled, Released,
	RestoreStarted, RestoreVerified, RestoreFailed,
	AttemptStarted, StopRequested, ResultSubmitted, Lost, Sealed, AttemptDone, Superseded,
}

// Kinds registers the lease and attempt event kinds (<transition>/v1, and
// OutputRefused) with the event store. A sealed attempt's checkpoint names its AttemptRecord
// blob, which must be published before the event references it (03). The
// store checks only that the blob exists: that it is a valid AttemptRecord
// of this attempt, token, base, project, task and contract revision is the
// wiring's check before the seal is written.
func Kinds() eventstore.Kinds {
	k := eventstore.Kinds{}
	for _, t := range transitions {
		k[t+"/v1"] = eventstore.Kind{Schema: EventSchema, BlobFields: []string{"checkpoint", "blob"}}
	}
	k[OutputRefused] = eventstore.Kind{Schema: OutputRefusalSchema}
	k[RestoreRecorded] = eventstore.Kind{Schema: RestoreSchema}
	return k
}
