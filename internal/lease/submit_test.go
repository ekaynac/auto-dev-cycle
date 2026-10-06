// SPDX-License-Identifier: Apache-2.0

package lease

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// A refused start releases the granted lease before any attempt exists;
// the lease then ends at released (05).
func TestStartRefused(t *testing.T) {
	w := openWorld(t)
	w.granted(requested())
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 2, LeaseEvent{Kind: StartRefused}, w.h)
	}); err == nil {
		t.Fatal("a refused start without its reason: accepted")
	}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 2, LeaseEvent{Kind: StartRefused, Reason: vocab.ReasonPreconditionFailed.String()}, w.h)
	})
	if l, _ := get[types.Lease](w, LeaseProjection, string(leaID)); l.State != types.LeaseStateReleasing || l.Attempt != nil {
		t.Fatalf("after start_refused: %s, attempt %v", l.State, l.Attempt)
	}
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaID, 3, attA, base) }); err == nil {
		t.Error("an attempt started under a refused lease")
	}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 3, event(Released), w.h)
	})
	if l, _ := get[types.Lease](w, LeaseProjection, string(leaID)); l.State != types.LeaseStateReleased {
		t.Fatalf("after released: %s", l.State)
	}
}

// A submission is fenced, carries its result and closes publication; a
// stale one proposes nothing; a result naming an unpublished blob is
// refused by the store.
func TestSubmitResult(t *testing.T) {
	w := openWorld(t)
	w.granted(requested())
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaID, 2, attA, base) })
	submit := func(token int64, r types.SubmittedResult) error {
		return w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return SubmitResult(tx, attA, token, r) })
	}
	if err := submit(2, submitted); !isFenced(err) {
		t.Errorf("another token's submission: %v", err)
	}
	unpublished := submitted
	unpublished.Blob = ptr(types.Digest("sha256:" + strings.Repeat("e", 64)))
	if err := submit(1, unpublished); err == nil {
		t.Error("a result naming an unpublished blob: accepted")
	}
	r := submitted
	r.Blob = ptr(w.put([]byte("the patch")))
	if err := submit(1, r); err != nil {
		t.Fatalf("the current attempt's submission: %v", err)
	}
	a, _ := get[types.Attempt](w, AttemptProjection, string(attA))
	if a.Status != types.AttemptStatusFinishing || a.Publication != types.AttemptPublicationClosed {
		t.Fatalf("after the submission: %s, %s", a.Status, a.Publication)
	}
	if err := submit(1, r); !isFenced(err) {
		t.Errorf("a second submission: %v", err)
	}
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return SubmitResult(tx, attB, 1, r) }); !isFenced(err) {
		t.Errorf("an unknown attempt's submission: %v", err)
	}
	// Nothing to lose once it is not running.
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return LoseRunning(tx, attA, types.AttemptLostReasonValueAgentDisconnect)
	})
	if a, _ := get[types.Attempt](w, AttemptProjection, string(attA)); a.Status != types.AttemptStatusFinishing {
		t.Fatalf("LoseRunning changed a finishing attempt: %s", a.Status)
	}
	if diffs, err := w.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %v", diffs, err)
	}
}

func isFenced(err error) bool {
	var s *Stale
	return errors.As(err, &s)
}

// A refusal leaves the attempt record as it is and makes the attempt
// untrustworthy, also after a rebuild; LoseRunning closes publication.
func TestRefusalsMakeAnAttemptUntrustworthy(t *testing.T) {
	w := openWorld(t)
	w.granted(requested())
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaID, 2, attA, base) })
	untrusted := func() bool {
		var u bool
		w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			var err error
			u, err = Untrustworthy(tx, attA)
			return nil, err
		})
		return u
	}
	if untrusted() {
		t.Fatal("untrustworthy before any refusal")
	}
	h := types.AttemptOutputRefusalHandle{Attempt: attA, Handle: "patch.diff", Digest: dgA, Size: 3}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		ev, r, err := Refusal(attA, h, types.AttemptOutputRefusalCheckSymlink, strings.Repeat("é", 600))
		if err == nil && (r.Reason != types.AttemptOutputRefusalReasonUnsafeOutput || len(r.Detail) > maxDetail) {
			t.Errorf("refusal %s, detail of %d bytes", r.Reason, len(r.Detail))
		}
		lost, lerr := LoseRunning(tx, attA, types.AttemptLostReasonValueAgentDisconnect)
		return append(lost, ev), errors.Join(err, lerr)
	})
	if !untrusted() {
		t.Fatal("not untrustworthy after a refusal")
	}
	if a, _ := get[types.Attempt](w, AttemptProjection, string(attA)); a.Status != types.AttemptStatusLost || a.Publication != types.AttemptPublicationClosed {
		t.Fatalf("after LoseRunning: %s, %s", a.Status, a.Publication)
	}
	// A stale refusal may follow any state.
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		ev, r, err := Refusal(attA, h, types.AttemptOutputRefusalCheckStaleAttempt, "")
		if r.Reason != types.AttemptOutputRefusalReasonStaleAttempt || r.Detail == "" {
			t.Errorf("stale refusal %+v", r)
		}
		return []eventstore.Event{ev}, err
	})
	if diffs, err := w.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %v", diffs, err)
	}
	if !untrusted() {
		t.Fatal("not untrustworthy after the rebuild")
	}
}
