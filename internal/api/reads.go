// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Read paths (08 "Reads"): one resource per path segment, and the caller's
// own identity, which is no grant domain (14 "Read scope").
const (
	ReadPath   = "/v1/{resource}"
	WhoamiPath = "/v1/whoami"
)

// A Resource is one GET resource. Its records are authorized one by one
// (04 "Read authorization"): a record is returned only when the caller's
// read scope, taken in the same snapshot, reads Domain in the record's
// authoritative project. A blob is never a resource: it is reached only
// through a readable record that references it (04:49-51).
type Resource struct {
	// Domain is the grant read kind the records are, singular (grant,
	// proposal, seat, note, project, hold, ...). It is declared here,
	// never derived from the path.
	Domain types.Name
	// List is every record, read in the snapshot.
	List func(tx *eventstore.Tx) ([]any, error)
	// Project is a record's authoritative project in the same snapshot;
	// nil is installation-wide, read only under an every-project read.
	Project func(tx *eventstore.Tx, record any) (*types.ProjectID, error)
	// Fields is the record's response allow-list (dotted paths, as the
	// event stream's): any other field is never sent.
	Fields []string
	// Schema is the schema file of one record as sent (api/<name>.json),
	// which the API description lists the resource with.
	Schema string
}

// Reads serves ReadPath over Resources and WhoamiPath, from Store.
type Reads struct {
	Store     *eventstore.Store
	Now       func() time.Time
	Gate      *Slots // the listener's bearer verifications; nil admits no one
	Resources map[string]Resource
}

var errNotGranted = errors.New("api: no grant in force grants reads")

var listeners = map[identity.Listener]types.WhoamiListener{identity.PrivateSocket: types.WhoamiListenerPrivateSocket,
	identity.SharedSocket: types.WhoamiListenerSharedSocket, identity.HTTPS: types.WhoamiListenerHttps}

// Resource serves ReadPath: unknown resources are 404, and no client
// parameter narrows or widens what the caller's scope reads.
func (h Reads) Resource(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("resource")
	res, ok := h.Resources[name]
	if !ok || res.Domain == "" || res.List == nil || res.Project == nil {
		http.NotFound(w, r)
		return
	}
	h.view(w, r, func(tx *eventstore.Tx, _ identity.Principal, _ types.Actor, scope authority.ReadScope) (any, error) {
		if scope.Empty() {
			return nil, errNotGranted
		}
		records, err := res.List(tx)
		if err != nil {
			return nil, err
		}
		out := types.ReadList{Resource: name, Outcome: types.ReadListOutcomeEmpty, Items: []json.RawMessage{}}
		for _, rec := range records {
			project, err := res.Project(tx, rec)
			if err != nil {
				return nil, err
			}
			if !scope.ReadsRecord(res.Domain, project) {
				continue
			}
			item, err := allowed(rec, res.Fields)
			if err != nil {
				return nil, err
			}
			out.Items = append(out.Items, item)
			out.Outcome = types.ReadListOutcomeFound
		}
		return out, nil
	})
}

// allowed is rec with only the allow-listed paths.
func allowed(rec any, fields []string) (json.RawMessage, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil || doc == nil {
		return nil, fmt.Errorf("api: a record that is no object: %w", err)
	}
	return json.Marshal(pick(doc, fields))
}

// Whoami serves WhoamiPath: any authenticated caller reads its own.
func (h Reads) Whoami(w http.ResponseWriter, r *http.Request) {
	h.view(w, r, func(_ *eventstore.Tx, p identity.Principal, a types.Actor, scope authority.ReadScope) (any, error) {
		out := types.Whoami{Actor: a.ID, Kind: types.WhoamiKind(a.Kind), Token: p.Token(), Listener: listeners[p.Via()],
			Reads: types.WhoamiReads{Everything: scope.Everything(), Grants: []types.WhoamiReadsGrantsItem{}}}
		for _, g := range scope.Reach() {
			out.Reads.Grants = append(out.Reads.Grants, types.WhoamiReadsGrantsItem{Grant: g.Grant, Projects: g.Projects, Kinds: g.Kinds})
		}
		return out, nil
	})
}

// view authenticates r's bearer on the listener that admitted it (through
// that listener's verification slots), takes the caller's read scope and
// runs read, all in one store snapshot, and writes what read returns.
func (h Reads) view(w http.ResponseWriter, r *http.Request,
	read func(*eventstore.Tx, identity.Principal, types.Actor, authority.ReadScope) (any, error)) {
	if r.URL.RawQuery != "" {
		http.Error(w, "reads take no query parameters", http.StatusBadRequest)
		return
	}
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	peer, _ := PeerOf(r.Context()) // none recorded: no listener, which authenticates nothing
	leave, ok := h.Gate.enter(r)
	if !ok {
		refuseBusy(w)
		return
	}
	var out any
	var authErr error
	err := func() error {
		defer leave()
		return h.Store.View(r.Context(), func(tx *eventstore.Tx) error {
			p, verified := principalOf(r)
			var err error
			if !verified {
				p, err = identity.Authenticated(tx, bearer, peer.Via)
			}
			var a types.Actor
			if err == nil {
				a, err = p.Live(tx)
			}
			if err != nil {
				authErr = err
				return nil
			}
			scope, err := authority.ReadScopeOf(tx, a, Events{Now: h.Now}.now())
			if err != nil {
				return err
			}
			out, err = read(tx, p, a, scope)
			return err
		})
	}()
	switch {
	case errors.Is(authErr, identity.ErrUnauthenticated):
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
	case errors.Is(err, errNotGranted):
		writeResult(w, http.StatusForbidden, vocab.Failed, vocab.ReasonActionNotGranted, "no grant in force grants reads")
	case err != nil || authErr != nil:
		writeResult(w, http.StatusServiceUnavailable, vocab.CouldNotRun, vocab.ReasonStoreUnreadable, "the event store could not be read")
	default:
		b, err := json.Marshal(out)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(append(b, '\n'))
	}
}
