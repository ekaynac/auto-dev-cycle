// SPDX-License-Identifier: Apache-2.0

// Package api is the controller HTTP API (docs/design/m0/08). This is its M0
// event stream: GET /v1/events as Server-Sent Events, resumable by seq,
// filtered server-side by the caller's read scope, and closed with
// failed/authority_revoked once the caller's authority no longer holds (04
// "Read authorization").
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// EventsPath is the stream's path.
const EventsPath = "/v1/events"

// batch bounds the events one poll reads.
const batch = 256

// Events serves the event stream from Store. Each poll re-checks the
// caller's authority and reads the next events in one view, so an event is
// never sent under a decision that did not see it.
type Events struct {
	Store *eventstore.Store
	Now   func() time.Time
	Poll  time.Duration // how long an idle stream waits before reading again (at most MaxPoll)
	Gate  *Slots        // caps concurrent bearer verifications; nil admits no one (Unlimited caps nothing)
}

// MaxPoll bounds how long a stream goes without a re-check (08).
const MaxPoll = time.Second

// poll is Poll within (0, MaxPoll]; unset or out of range is MaxPoll.
func (h Events) poll() time.Duration {
	if h.Poll <= 0 || h.Poll > MaxPoll {
		return MaxPoll
	}
	return h.Poll
}

// now is the server's clock: Now, or the wall clock when none is set (a
// zero time would put every grant out of force, never in it).
func (h Events) now() time.Time {
	if h.Now == nil {
		return time.Now()
	}
	return h.Now()
}

// filter is the caller's own narrowing of the stream (never its authority).
type filter struct {
	after   int64
	kinds   []string // kind names without their /v<n>, or whole kinds
	project string
}

func parseFilter(r *http.Request) (filter, error) {
	var f filter
	q := r.URL.Query()
	for k, v := range q {
		if !slices.Contains([]string{"after", "kinds", "project"}, k) || len(v) != 1 {
			return f, fmt.Errorf("query parameter %q: one of after, kinds, project, once", k)
		}
	}
	if s := q.Get("after"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			return f, errors.New("after: a seq, 0 or more")
		}
		f.after = n
	}
	if s := q.Get("project"); s != "" {
		if err := ids.Check(s, "prj"); err != nil {
			return f, fmt.Errorf("project: %w", err)
		}
		f.project = s
	}
	if s := q.Get("kinds"); s != "" {
		f.kinds = strings.Split(s, ",")
		if slices.Contains(f.kinds, "") {
			return f, errors.New("kinds: a comma-separated list of kinds")
		}
	}
	return f, nil
}

func (f filter) matches(e eventstore.Event) bool {
	name, _, _ := strings.Cut(e.Kind, "/")
	return (f.project == "" || (e.Project != nil && *e.Project == f.project)) &&
		(f.kinds == nil || slices.Contains(f.kinds, e.Kind) || slices.Contains(f.kinds, name))
}

func (h Events) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	f, err := parseFilter(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	var scope authority.ReadScope
	var authErr error
	var actor types.ActorID
	leave, ok := h.Gate.enter(r)
	if !ok {
		refuseBusy(w)
		return
	}
	err = func() error {
		defer leave()
		return h.Store.View(r.Context(), func(tx *eventstore.Tx) error {
			a, err := identity.Authenticate(tx, bearer)
			if err != nil {
				authErr = err
				return nil
			}
			actor = a.ID
			scope, err = authority.ReadScopeOf(tx, a, h.now())
			return err
		})
	}()
	switch {
	case errors.Is(authErr, identity.ErrUnauthenticated):
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	case err != nil || authErr != nil:
		writeResult(w, http.StatusServiceUnavailable, vocab.CouldNotRun, vocab.ReasonStoreUnreadable, "the event store could not be read")
		return
	case scope.Empty():
		writeResult(w, http.StatusForbidden, vocab.Failed, vocab.ReasonActionNotGranted, "no grant in force grants reads")
		return
	}
	// Every write is made under a deadline (08): a writer that cannot take
	// one is refused rather than streamed to without a bound.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	st := &streamState{h: h, w: w, rc: rc, scope: scope, bearer: bearer, actor: actor, abort: hardAbort(r), lastCheck: time.Now()}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	if err := st.bounded(func() error { w.WriteHeader(http.StatusOK); return rc.Flush() }); err != nil {
		return
	}
	st.run(r.Context(), f)
}

// streamState is one open stream: the caller's authority as it was opened,
// and when it was last checked.
type streamState struct {
	h         Events
	w         http.ResponseWriter
	rc        *http.ResponseController
	scope     authority.ReadScope
	bearer    string
	actor     types.ActorID
	abort     func()    // closes an HTTP/1.1 connection hard; nil otherwise
	lastCheck time.Time // wall clock of the last authority check
}

var (
	revoked = vocab.Result{Outcome: vocab.Failed, Reason: vocab.ReasonAuthorityRevoked,
		Detail: "the caller's token, actor or grant changed in a way that removes access"}
	unreadable = vocab.Result{Outcome: vocab.CouldNotRun, Reason: vocab.ReasonStoreUnreadable, Detail: "the event store could not be read"}
)

// nextCheck is when authority must be checked again: one poll bound after
// the last check, whatever writing took meanwhile.
func (st *streamState) nextCheck() time.Time { return st.lastCheck.Add(st.h.poll()) }

// authorize re-checks the caller's authority now, in tx: nil while it
// holds, else the result the stream closes with. Lost authority is
// revocation; a store that cannot be read is never read as either.
func (st *streamState) authorize(tx *eventstore.Tx) *vocab.Result {
	st.lastCheck = time.Now()
	if mutation.Is(mutation.StreamCheckedAtOpenOnly) {
		return nil
	}
	holds, err := st.scope.Holds(tx, st.h.now())
	if err != nil {
		return &unreadable
	}
	live := identity.StillLive(tx, st.bearer, st.actor)
	switch {
	case live != nil && !errors.Is(live, identity.ErrUnauthenticated):
		return &unreadable
	case live != nil || !holds:
		return &revoked
	}
	return nil
}

// recheck runs authorize in its own view.
func (st *streamState) recheck(ctx context.Context) *vocab.Result {
	var res *vocab.Result
	if err := st.h.Store.View(ctx, func(tx *eventstore.Tx) error { res = st.authorize(tx); return nil }); err != nil {
		return &unreadable
	}
	return res
}

// errCutOff: a write did not finish by its deadline.
var errCutOff = errors.New("api: the client did not take the write by its deadline")

// bounded runs write (a write and its flush) under a deadline at the next
// authority check, so no write outlasts a change of authority. At the
// deadline an HTTP/1.1 connection is closed hard, beneath TLS. A write
// that finishes clears the deadline before the stream idles: an armed one
// would reset a healthy idle HTTP/2 stream, and an expired one cannot be
// extended.
func (st *streamState) bounded(write func() error) error {
	deadline := st.nextCheck()
	if err := st.rc.SetWriteDeadline(deadline); err != nil {
		return err
	}
	var cut *time.Timer
	if st.abort != nil {
		cut = time.AfterFunc(time.Until(deadline), st.abort)
	}
	err := write()
	if cut != nil && !cut.Stop() {
		err = errors.Join(err, errCutOff)
	}
	if err != nil {
		return err
	}
	return st.rc.SetWriteDeadline(time.Time{})
}

// send writes and flushes one frame, bounded.
func (st *streamState) send(event, id string, v any) error {
	return st.bounded(func() error {
		if err := writeFrame(st.w, event, id, v); err != nil {
			return err
		}
		return st.rc.Flush()
	})
}

// run polls until the client leaves, stalls, or the caller's authority
// ends. Authority is checked with each batch it reads (in the same view)
// and again before each frame; an idle stream checks again one poll bound
// after the last check, so checks are never further apart than that.
func (st *streamState) run(ctx context.Context, f filter) {
	after := f.after
	end := func(r *vocab.Result) {
		if ctx.Err() == nil {
			_ = st.send("closed", "", r) // the stream ends either way
		}
	}
	for {
		var evs []eventstore.Event
		var res *vocab.Result
		err := st.h.Store.View(ctx, func(tx *eventstore.Tx) error {
			if res = st.authorize(tx); res != nil {
				return nil
			}
			var err error
			evs, err = tx.EventsAfter(after, batch)
			return err
		})
		if err != nil && res == nil {
			res = &unreadable
		}
		if res != nil {
			end(res)
			return
		}
		for _, e := range evs {
			after = e.Seq
			if !st.scope.Reads(e) || !f.matches(e) {
				continue
			}
			if res := st.recheck(ctx); res != nil {
				end(res)
				return
			}
			// A frame that cannot be written in time ends the stream
			// without a closed frame: the client resumes by seq.
			if err := st.send("event", strconv.FormatInt(e.Seq, 10), redact(e)); err != nil {
				return
			}
		}
		if len(evs) < batch {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(st.nextCheck())):
			}
		}
	}
}

// writeFrame writes one SSE frame; a value that does not marshal is never
// sent half-written.
func writeFrame(w http.ResponseWriter, event, id string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	head := ""
	if id != "" {
		head = "id: " + id + "\n"
	}
	_, err = fmt.Fprintf(w, "%sevent: %s\ndata: %s\n\n", head, event, b)
	return err
}

func writeResult(w http.ResponseWriter, status int, o vocab.Outcome, reason vocab.Reason, detail string) {
	b, err := json.Marshal(vocab.Result{Outcome: o, Reason: reason, Detail: detail})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}
