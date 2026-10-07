// SPDX-License-Identifier: Apache-2.0

package api

// The resource families (14 "Read scope", PR 9b-9e). Each lists its
// records from its projection in the read's snapshot and resolves a
// record's project by re-reading that projection by the record's id: the
// listed document is only what is sent (through Fields), never what
// decides who reads it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// Registered is the daemon's resources, by path segment; each resource
// family joins it with its projection.
func Registered() map[string]Resource {
	return map[string]Resource{"grants": grants, "proposals": proposals}
}

// grants are every grant record, revoked ones included: a narrowing is a
// revocation and a new grant (04 "Grants"), and a reader auditing its
// authority sees both. Expiry and revocation are in the record.
var grants = Resource{
	Domain: "grant",
	Schema: "api/grant.json",
	List: func(tx *eventstore.Tx) ([]any, error) {
		return listed(tx, authority.GrantProjection, func(doc map[string]any) (map[string]any, error) {
			g, ok := doc["grant"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("api: a grant record without its grant")
			}
			rec := map[string]any{"revoked_at": doc["revoked_at"]}
			for k, v := range g {
				rec[k] = v
			}
			return rec, nil
		})
	},
	Project: func(tx *eventstore.Tx, rec any) (*types.ProjectID, error) {
		var e types.GrantEvent
		if err := reread(tx, authority.GrantProjection, rec, func(doc map[string]any) string {
			actor, _ := doc["actor"].(string)
			id, _ := doc["id"].(string)
			return actor + "/" + id
		}, &e); err != nil {
			return nil, err
		}
		return grantProject(e.Grant), nil
	},
	// Never approval: the record a grant was issued under is the
	// approvals' to show.
	Fields: []string{"id", "actor", "projects", "actions", "reads", "valid_from", "expires_at", "confirmation_overrides", "issued_by", "revoked_at"},
}

// grantProject is a grant's project: the one project it names, in its
// projects and its listed read projects; naming several, or every project,
// makes it installation-wide (nil), for a reader of one project must not
// learn of another's.
func grantProject(g types.Grant) *types.ProjectID {
	if g.Projects.All {
		return nil
	}
	named := slices.Compact(slices.Sorted(slices.Values(slices.Concat(g.Projects.List, g.Reads.Projects.List))))
	if len(named) != 1 {
		return nil
	}
	return &named[0]
}

// proposals are every proposal, in any state. Never its scope, the
// command's raw input (a later action's input may carry a secret), nor a
// digest that may depend on what it withholds: its binding digest (of that
// input), its expected state, or its object's content digest (the object
// of a proposal.confirm is another proposal, its scope included).
var proposals = Resource{
	Domain: "proposal",
	Schema: "api/proposal.json",
	List: func(tx *eventstore.Tx) ([]any, error) {
		return listed(tx, authority.ProposalProjection, func(doc map[string]any) (map[string]any, error) { return doc, nil })
	},
	Project: func(tx *eventstore.Tx, rec any) (*types.ProjectID, error) {
		var p types.Proposal
		err := reread(tx, authority.ProposalProjection, rec, func(doc map[string]any) string {
			id, _ := doc["id"].(string)
			return id
		}, &p)
		return p.Project, err
	},
	Fields: []string{"id", "action", "change_class", "project", "object.type", "object.id", "object.version", "proposed_by", "grantee", "policy", "status", "invalidated_reason", "created_at", "expires_at"},
}

// listed is each record of projection, as of, in key order; keys holding
// another record type (a proposal's approval/... entries) are not records.
func listed(tx *eventstore.Tx, projection string, as func(map[string]any) (map[string]any, error)) ([]any, error) {
	keys, err := tx.Keys(projection, "")
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, k := range keys {
		if projection == authority.ProposalProjection && strings.Contains(k, "/") {
			continue
		}
		raw, _, err := tx.Get(projection, k)
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil || doc == nil {
			return nil, fmt.Errorf("api: %s record %s is damaged: %v", projection, k, err)
		}
		rec, err := as(doc)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// reread loads into v the record of projection under the key rec names:
// what its project is read from. A key with no record is damage.
func reread(tx *eventstore.Tx, projection string, rec any, key func(map[string]any) string, v any) error {
	doc, _ := rec.(map[string]any)
	k := key(doc)
	raw, ok, err := tx.Get(projection, k)
	if err == nil && !ok {
		err = fmt.Errorf("api: no %s record %s", projection, k)
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}
