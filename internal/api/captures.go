// SPDX-License-Identifier: Apache-2.0

package api

// Egress captures (docs/design/m1/00-slice-plan.md §8 item 4; P22a-5a):
// during an exit run only, the gateway keeps each request body it sends,
// before the credential is injected, as a restricted blob in the
// controller's capture store (a blob store of its own, which no route
// serves and no input names).
//   - RecordCapture records it, as the system actor, as blob.capture_kept
//     with its call, class (restricted) and expiry (7 days, P7D); the
//     captures projection keeps it until its purge.
//   - PurgeCaptures, in the controller's purge loop, removes every capture
//     expired at now: its bytes first, then blob.capture_purged under a key
//     bound to its record, so a crash in between repeats the removal (a
//     blob already gone is no error) and the commit replays, adding no
//     second event. A digest another unpurged capture still holds keeps its
//     bytes until that one's purge.
//   - Both events stream their fields (none is secret); the body never.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

const (
	CaptureKept        = "blob.capture_kept/v1"
	CapturePurged      = "blob.capture_purged/v1"
	CaptureAction      = "blob.capture" // the system actions that record them; no bearer sends either
	CapturePurgeAction = "blob.purge_capture"
	CaptureProjection  = "captures" // capture event id → its record, until purged
	captureSchema      = "records/egress-capture"
	captureClass       = gateway.ClassRestricted
)

// CaptureKinds are the capture events.
func CaptureKinds() eventstore.Kinds {
	return eventstore.Kinds{CaptureKept: {Schema: captureSchema}, CapturePurged: {Schema: captureSchema}}
}

func init() {
	for k := range CaptureKinds() {
		streamable[k] = []string{"digest", "lease", "attempt", "call", "class", "expires_at"}
	}
}

// keptCapture is a capture's projected record: its kept event's id and
// payload.
type keptCapture struct {
	ID     string              `json:"id"`
	Record types.EgressCapture `json:"record"`
}

// ProjectCaptures folds the capture events: a kept one is put under its
// event id, a purged one removes the capture it names (its subject).
func ProjectCaptures(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		switch ev.Kind {
		case CaptureKept:
			var r types.EgressCapture
			if err := json.Unmarshal(ev.Payload, &r); err != nil {
				return err
			}
			if ev.Subject != ev.ID || r.Class != captureClass {
				return fmt.Errorf("api: capture %s: its own subject, class restricted", ev.ID)
			}
			raw, err := json.Marshal(keptCapture{ID: ev.ID, Record: r})
			if err == nil {
				err = tx.Put(CaptureProjection, ev.ID, raw)
			}
			if err != nil {
				return err
			}
		case CapturePurged:
			if _, ok, err := tx.Get(CaptureProjection, ev.Subject); err != nil || !ok {
				return errors.Join(err, fmt.Errorf("api: purge of capture %s, which is not kept", ev.Subject))
			}
			if err := tx.Delete(CaptureProjection, ev.Subject); err != nil {
				return err
			}
		}
	}
	return nil
}

// RecordCapture records c, kept, as the system actor.
func RecordCapture(ctx context.Context, store *eventstore.Store, system identity.Principal, c gateway.Captured) error {
	r := types.EgressCapture{Digest: types.Digest(c.Digest), Lease: c.Lease, Attempt: c.Attempt, Call: c.Call, Class: c.Class,
		ExpiresAt: types.Time(c.ExpiresAt.UTC().Format(time.RFC3339Nano))}
	payload, perr := json.Marshal(r)
	evt, eerr := ids.New("evt")
	cmd, cerr := ids.New("cmd")
	if err := errors.Join(perr, eerr, cerr); err != nil {
		return err
	}
	_, err := store.SubmitApplied(ctx, system.Command(cmd, CaptureAction, cmd, payload, nil),
		eventstore.Applies(func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			return []eventstore.Event{{ID: evt, Kind: CaptureKept, Subject: evt, Payload: payload}}, json.RawMessage(`{}`), nil
		}))
	return err
}

// Remover is what PurgeCaptures needs of the capture store.
type Remover interface{ Remove(digest string) error }

// PurgeCaptures purges as system every capture expired at now, and returns
// how many it purged; one whose purge fails is left to the next run, the
// others purged, and every failure returned.
func PurgeCaptures(ctx context.Context, store *eventstore.Store, blobs Remover, system identity.Principal, now time.Time, log io.Writer) (int, error) {
	var kept []keptCapture
	if err := store.View(ctx, func(tx *eventstore.Tx) error {
		keys, err := tx.Keys(CaptureProjection, "")
		for _, k := range keys {
			raw, _, gerr := tx.Get(CaptureProjection, k)
			var c keptCapture
			if err = errors.Join(err, gerr, json.Unmarshal(raw, &c)); err != nil {
				return err
			}
			kept = append(kept, c)
		}
		return err
	}); err != nil {
		return 0, err
	}
	live := map[types.Digest]int{} // unexpired captures by digest: their bytes stay
	for _, c := range kept {
		if !expired(c.Record, now) {
			live[c.Record.Digest]++
		}
	}
	n, failed := 0, []error{}
	for _, c := range kept {
		if !expired(c.Record, now) {
			continue
		}
		if ctx.Err() != nil {
			return n, errors.Join(append(failed, ctx.Err())...)
		}
		var err error
		if live[c.Record.Digest] == 0 {
			err = blobs.Remove(string(c.Record.Digest))
		}
		if err == nil {
			err = recordCapturePurge(ctx, store, system, c)
		}
		if err != nil {
			failed = append(failed, fmt.Errorf("purging capture %s: %w", c.ID, err))
			continue
		}
		n++
	}
	if len(failed) > 0 && log != nil {
		fmt.Fprintf(log, "adcd: captures: %v\n", errors.Join(failed...))
	}
	return n, errors.Join(failed...)
}

// expired: c's expiry is at or before now; an unreadable expiry is never.
func expired(c types.EgressCapture, now time.Time) bool {
	at, err := time.Parse(time.RFC3339Nano, string(c.ExpiresAt))
	return err == nil && !now.Before(at)
}

// recordCapturePurge commits c's purge event, or replays it.
func recordCapturePurge(ctx context.Context, store *eventstore.Store, system identity.Principal, c keptCapture) error {
	payload, perr := json.Marshal(c.Record)
	evt, eerr := ids.New("evt")
	cmd, cerr := ids.New("cmd")
	if err := errors.Join(perr, eerr, cerr); err != nil {
		return err
	}
	_, err := store.Submit(ctx, system.Command(cmd, CapturePurgeAction, "purge-capture/"+c.ID, payload, nil),
		eventstore.Applies(func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			return []eventstore.Event{{ID: evt, Kind: CapturePurged, Subject: c.ID, Payload: payload}}, json.RawMessage(`{}`), nil
		}))
	return err
}
