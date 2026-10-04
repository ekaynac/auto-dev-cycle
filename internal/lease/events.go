// SPDX-License-Identifier: Apache-2.0

package lease

import "github.com/ekaynac/auto-dev-cycle/internal/eventstore"

// EventSchema is the payload schema of every lease and attempt event.
const EventSchema = "records/lease-event"

// transitions are the event names of note 05's tables, lease and attempt.
var transitions = []string{
	Requested, Waiting, Granted, Expired, Completed, Revoked, Preempted, Settled, Released,
	RestoreStarted, RestoreVerified, RestoreFailed,
	AttemptStarted, StopRequested, ResultSubmitted, Lost, Sealed, AttemptDone, Superseded,
}

// Kinds registers the lease and attempt event kinds (<transition>/v1) with
// the event store. A sealed attempt's checkpoint names its AttemptRecord
// blob, which must be published before the event references it (03).
func Kinds() eventstore.Kinds {
	k := eventstore.Kinds{}
	for _, t := range transitions {
		k[t+"/v1"] = eventstore.Kind{Schema: EventSchema, BlobFields: []string{"checkpoint"}}
	}
	return k
}
