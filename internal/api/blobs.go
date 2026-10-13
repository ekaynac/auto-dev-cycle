// SPDX-License-Identifier: Apache-2.0

package api

// Blob uploads (docs/design/m1/00-slice-plan.md P2): POST /v1/blobs stores
// the body as an operator's or a supervisor's upload, within the
// blobstore's bounds; GET lists the live uploads, for an operator; any
// other method is 405. No route serves an upload's bytes. An expired upload
// is purged by the system actor, with a purge event.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	BlobsPath   = "/v1/blobs"
	BlobUploads = "blob-uploads"          // the resource GET BlobsPath lists
	BlobPurged  = "blob.upload_purged/v1" // an expired upload's purge
	PurgeAction = "blob.purge_upload"     // the system action that records it; no bearer sends it
)

// BlobKinds are the upload events.
func BlobKinds() eventstore.Kinds {
	return eventstore.Kinds{BlobPurged: {Schema: "records/blob-upload-purged"}}
}

// A purge event streams only what an upload was, never whose it was; its
// subject is the event itself, so no actor's version moves (recordPurge).
func init() { streamable[BlobPurged] = []string{"digest", "expires_at"} }

// Blobs serves BlobsPath, on Now's clock (nil: time.Now); a fault's or a
// skipped entry's detail goes to Log only.
type Blobs struct {
	Store *eventstore.Store
	Blobs *blobstore.Store
	Gate  *Slots
	Now   func() time.Time
	Log   io.Writer
}

func (h Blobs) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, blobstore.MaxUploadBytes) // the route's own cap: the listener's wrapper leaves this route to it
	p, err := Authenticate(h.Store, r, h.Gate)
	var a types.Actor
	if err == nil {
		err = h.Store.View(r.Context(), func(tx *eventstore.Tx) (err error) {
			a, err = p.Live(tx)
			return err
		})
	}
	var tooLarge *http.MaxBytesError
	switch {
	case r.URL.RawQuery != "":
		http.Error(w, "blob requests take no query parameters", http.StatusBadRequest)
	case errors.Is(err, ErrBusy):
		refuseBusy(w)
	case errors.Is(err, identity.ErrUnauthenticated):
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
	case err != nil || h.Blobs == nil:
		h.fault(w, errors.Join(err, errors.New("authenticating")))
	case r.Method == http.MethodGet && a.Kind != types.ActorKindOperator:
		writeResult(w, http.StatusForbidden, vocab.Failed, vocab.ReasonOperatorOnly, "only an operator lists uploads")
	case r.Method == http.MethodGet:
		us, skipped := h.Blobs.Uploads()
		h.logSkipped(skipped)
		out := types.ReadList{Resource: BlobUploads, Outcome: types.ReadListOutcomeEmpty, Items: []json.RawMessage{}}
		for _, u := range us {
			b, merr := json.Marshal(Record(u))
			out.Items, out.Outcome, err = append(out.Items, b), types.ReadListOutcomeFound, errors.Join(err, merr)
		}
		h.answer(w, out, err)
	case a.Kind != types.ActorKindOperator && a.Kind != types.ActorKindSupervisor:
		writeResult(w, http.StatusForbidden, vocab.Failed, vocab.ReasonActorKindRefused, "only operators and supervisors upload blobs")
	default:
		u, err := h.Blobs.Upload(string(a.ID), r.Body, h.now())
		if errors.Is(err, blobstore.ErrUploadBound) || errors.As(err, &tooLarge) {
			writeResult(w, http.StatusRequestEntityTooLarge, vocab.Failed, vocab.ReasonUploadBoundExceeded, "the upload is over a bound; nothing was stored")
			return
		}
		h.answer(w, Record(u), err)
	}
}

func (h Blobs) now() time.Time {
	if h.Now == nil {
		return time.Now()
	}
	return h.Now()
}

func (h Blobs) logSkipped(skipped error) {
	if skipped != nil && h.Log != nil {
		fmt.Fprintf(h.Log, "adcd: uploads: skipped: %v\n", skipped)
	}
}

func (h Blobs) answer(w http.ResponseWriter, v any, err error) {
	b, merr := json.Marshal(v)
	if err = errors.Join(err, merr); err != nil {
		h.fault(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(append(b, '\n'))
}

func (h Blobs) fault(w http.ResponseWriter, err error) {
	if h.Log != nil {
		fmt.Fprintf(h.Log, "adcd: blobs: %v\n", err)
	}
	writeResult(w, http.StatusServiceUnavailable, vocab.CouldNotRun, vocab.ReasonStoreUnreadable, "the upload store could not be used; the controller log has the detail")
}

// Record is an upload as sent and as its purge event records it.
func Record(u blobstore.Upload) types.BlobUpload {
	at := func(t time.Time) types.Time { return t.UTC().Format(time.RFC3339Nano) }
	return types.BlobUpload{Digest: types.Digest(u.Digest), Size: u.Size, Actor: types.ActorID(u.Actor),
		UploadedAt: at(u.UploadedAt), ExpiresAt: at(u.UploadedAt.Add(blobstore.UploadTTL))}
}

// Purger is what PurgeExpired needs of the blob store.
type Purger interface {
	Uploads() ([]blobstore.Upload, error)
	Tombstones() ([]blobstore.Upload, error)
	Tombstone(blobstore.Upload) (bool, error)
	Bury(blobstore.Upload) error
}

// PurgeExpired purges as system every upload expired at now, first
// finishing the purges a crash left set aside. Each is set aside (so no
// claim or renewal sees it), then its purge event is committed under a key
// bound to the upload, and only then its bytes removed: a crash in between
// leaves a tombstone, whose purge the next run repeats as a replay, adding
// no second event. Entries that cannot be read are skipped and logged to
// log; an upload whose purge fails is left to the next run, the others
// purged, and every failure returned. It returns how many it purged.
func PurgeExpired(ctx context.Context, store *eventstore.Store, blobs Purger, system identity.Principal, now time.Time, log io.Writer) (int, error) {
	tombs, terr := blobs.Tombstones()
	live, lerr := blobs.Uploads()
	Blobs{Log: log}.logSkipped(errors.Join(terr, lerr))
	n, failed := 0, []error{}
	for i, u := range append(tombs, live...) {
		if ctx.Err() != nil {
			return n, errors.Join(append(failed, ctx.Err())...)
		}
		if i >= len(tombs) { // a live upload: set it aside first, if expired
			if now.Before(u.UploadedAt.Add(blobstore.UploadTTL)) {
				continue
			}
			set, err := blobs.Tombstone(u)
			if err != nil || !set {
				failed = append(failed, err)
				continue
			}
		}
		err := recordPurge(ctx, store, system, u)
		if err == nil {
			err = blobs.Bury(u)
		}
		if err != nil {
			failed = append(failed, fmt.Errorf("purging %s of %s: %w", u.Digest, u.Actor, err))
			continue
		}
		n++
	}
	return n, errors.Join(failed...)
}

// recordPurge commits u's purge event, or replays it. The event is its own
// subject: an upload is no object, and no actor's version may move.
func recordPurge(ctx context.Context, store *eventstore.Store, system identity.Principal, u blobstore.Upload) error {
	payload, perr := json.Marshal(Record(u))
	evt, eerr := ids.New("evt")
	cmd, cerr := ids.New("cmd")
	if err := errors.Join(perr, eerr, cerr); err != nil {
		return err
	}
	key := fmt.Sprintf("purge/%s/%s/%d", u.Actor, u.Digest, u.UploadedAt.UnixNano())
	_, err := store.Submit(ctx, system.Command(cmd, PurgeAction, key, payload, nil),
		eventstore.Applies(func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			return []eventstore.Event{{ID: evt, Kind: BlobPurged, Subject: evt, Payload: payload}}, payload, nil
		}))
	return err
}
