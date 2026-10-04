// SPDX-License-Identifier: Apache-2.0

package lease

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// payload is a lease event as the store would carry it.
func payload(t *testing.T, transition, reason string, l *types.Lease, a *types.Attempt) []byte {
	t.Helper()
	var r *string
	if reason != "" {
		r = &reason
	}
	b, err := json.Marshal(struct {
		Transition string         `json:"transition"`
		Reason     *string        `json:"reason"`
		Lease      *types.Lease   `json:"lease"`
		Attempt    *types.Attempt `json:"attempt"`
	}{transition, r, l, a})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every transition of both tables has a kind, and each kind's payload,
// built from the state machine's own output, fits the schema.
func TestEventKinds(t *testing.T) {
	kinds := Kinds()
	if len(kinds) != 19 {
		t.Fatalf("%d kinds, want 19", len(kinds))
	}
	h := holds(t)
	for _, tr := range transitions {
		if _, ok := kinds[tr+"/v1"]; !ok {
			t.Errorf("%s has no kind", tr)
		}
		var l *types.Lease
		var a *types.Attempt
		reason := ""
		switch {
		case tr == AttemptStarted:
			x := reach(t, types.LeaseStateActive, false)
			y := Start(attA, untID, leaID, base)
			l, a = &x, &y
		case strings.HasPrefix(tr, "lease."):
			from := map[string]types.LeaseState{Requested: "requested", Waiting: "requested", Granted: "requested", Expired: "granted",
				Completed: "active", Revoked: "active", Preempted: "active", Settled: "settling", Released: "releasing",
				RestoreStarted: "settling", RestoreVerified: "restoring", RestoreFailed: "restoring"}[tr]
			target := strings.HasPrefix(tr, "lease.restore")
			if tr == Requested {
				x := requested() // the first request
				l = &x
				break
			}
			x, err := ApplyLease(reach(t, from, target), event(tr), target, h)
			if err != nil {
				t.Fatalf("%s: %v", tr, err)
			}
			l, reason = &x, event(tr).Reason
		case tr == Superseded:
			old, _, err := Supersede(reachAttempt(t, types.AttemptStatusSealed), attB, leaID, base)
			if err != nil {
				t.Fatal(err)
			}
			a = &old
		default:
			from := map[string]types.AttemptStatus{StopRequested: "running", ResultSubmitted: "running", Lost: "running",
				Sealed: "lost", AttemptDone: "sealed"}[tr]
			x, err := ApplyAttempt(reachAttempt(t, from), attemptEvent(tr))
			if err != nil {
				t.Fatalf("%s: %v", tr, err)
			}
			a = &x
		}
		if err := contract.Validate(EventSchema, payload(t, tr, reason, l, a)); err != nil {
			t.Errorf("%s: %v", tr, err)
		}
	}
}

func TestEventPayloadRules(t *testing.T) {
	active := reach(t, types.LeaseStateActive, false)
	running := Start(attA, untID, leaID, base)
	for name, p := range map[string][]byte{
		"a revocation without its reason": payload(t, Revoked, "", &active, nil),
		"a reason on a grant":             payload(t, Granted, "because", &active, nil),
		"a lease event without the lease": payload(t, Granted, "", nil, nil),
		"a lease event with an attempt":   payload(t, Granted, "", &active, &running),
		"a start without the lease":       payload(t, AttemptStarted, "", nil, &running),
		"an attempt event with the lease": payload(t, StopRequested, "", &active, &running),
		"an attempt event without it":     payload(t, Lost, "", nil, nil),
	} {
		if err := contract.Validate(EventSchema, p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Through the store: a seal's checkpoint is a blob reference, refused until
// the AttemptRecord is published.
func TestSealNeedsItsPublishedRecord(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	lock, err := eventstore.AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := eventstore.Open(ctx, lock, eventstore.Config{Kinds: Kinds(), Blobs: blobs,
		Hooks: eventstore.Hooks{Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil }}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	record, err := contract.Encode("records/attempt-record", types.AttemptRecord{Attempt: attA, Project: "prj_01m3250vkwffc979nzjw0g2sf9",
		Task: "tsk_01m3250v9xft08ssr4ygq6g2np", ContractRevision: "7", FencingToken: 1, BaseTree: base.Tree,
		FailingTests: []string{}, Trail: "sha256:" + types.Digest(strings.Repeat("b", 64)), EvidenceManifest: "sha256:" + types.Digest(strings.Repeat("c", 64)),
		Seal: types.AttemptRecordSeal{Reason: "abrupt_loss", At: "2026-10-05T12:00:00Z", SealedBy: "act_01m3250v0aer9ss2097svmefj8"}})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(record)
	digest := types.Digest("sha256:" + hex.EncodeToString(sum[:]))
	sealed, err := ApplyAttempt(reachAttempt(t, types.AttemptStatusLost), AttemptEvent{Kind: Sealed, Checkpoint: digest})
	if err != nil {
		t.Fatal(err)
	}
	submit := func(key string) error {
		cmd, err := ids.New("cmd")
		if err != nil {
			t.Fatal(err)
		}
		evt, err := ids.New("evt")
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Submit(ctx, eventstore.Command{ID: cmd, Actor: "act_01m3250v0aer9ss2097svmefj8", IdempotencyKey: key, Input: []byte(`{}`)},
			func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				return []eventstore.Event{{ID: evt, Kind: Sealed + "/v1", Subject: string(sealed.ID), Payload: payload(t, Sealed, "", nil, &sealed)}}, json.RawMessage(`{}`), nil
			})
		return err
	}
	if err := submit("seal-1"); err == nil {
		t.Fatal("a seal referencing an unpublished AttemptRecord was accepted")
	}
	if _, err := blobs.Put(bytes.NewReader(record), string(digest)); err != nil {
		t.Fatal(err)
	}
	if err := submit("seal-2"); err != nil {
		t.Fatalf("a seal referencing its published AttemptRecord: %v", err)
	}
}
