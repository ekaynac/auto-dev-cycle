// SPDX-License-Identifier: Apache-2.0

// Package brief is the plan node's result in M1 (P22a-5c-3; #226 entries
// 103 and 117): the plan brief, a blob the controller publishes from the
// task's own record, holding only its spec's title and requirement (never
// its acceptance) and naming the spec's digest and the project's data
// policy's, and its ref, a root (no lineage) in the project's scope under
// that policy.
//
// The ref is honoured only as its own task's brief: intent's I18 walk
// requires its content digest to be the digest of Of over the task's own
// record and the project's data policy now, its one restriction that
// policy, and its scope the project, so any other bytes (another task's,
// another spec's, a requirement that differs, another policy's) are
// refused, never walked through. A policy changed since the plan makes
// another brief, so the planned one is refused: today the operator
// revokes the task's approval, which abandons it (no re-plan exists).
package brief

import (
	"cmp"
	"encoding/json"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
)

// Schema is a brief's schema field; record its schema file.
const (
	Schema = "adc.plan-brief/v1"
	record = "records/plan-brief"
)

// Of is task t's plan brief under the data policy of digest policy
// (Restriction's PolicyDigest), its canonical bytes (RFC 8785), and their
// digest: the spec's title and requirement, its digest, and policy.
func Of(t types.Task, policy types.Digest) ([]byte, types.Digest, error) {
	raw, err := contract.Encode(record, types.PlanBrief{Schema: Schema, Task: t.ID, SpecDigest: t.SpecDigest, PolicyDigest: policy,
		Title: t.Spec.Title, Requirement: t.Spec.Requirement})
	if err != nil {
		return nil, "", err
	}
	b, err := eventstore.Canonical(raw)
	if err != nil {
		return nil, "", err
	}
	d, err := eventstore.ContentDigest(b)
	return b, types.Digest(d), err
}

// Restriction is project's data policy as a ref's restriction, from policy,
// the bytes of the data_policy its recorded profile holds
// (authority.ProjectDataPolicy): their digest (authority.PolicyDigest,
// never over a re-encoded struct; Opus 22a-5c-3 r2 M1), floor, routing and
// allowed destinations.
func Restriction(project types.ProjectID, policy json.RawMessage) (types.Restriction, error) {
	var dp types.ProjectProfileDataPolicy
	if err := json.Unmarshal(policy, &dp); err != nil {
		return types.Restriction{}, err
	}
	d, err := authority.PolicyDigest(policy)
	if err != nil {
		return types.Restriction{}, err
	}
	dest := make([]string, 0, len(dp.AllowedDestinations))
	for _, s := range dp.AllowedDestinations {
		dest = append(dest, string(s))
	}
	slices.SortFunc(dest, cmp.Compare[string])
	return types.Restriction{Project: project, PolicyDigest: types.Digest(d), Floor: types.RestrictionFloor(dp.Floor),
		Routing: types.RestrictionRouting(dp.Routing), AllowedDestinations: slices.Compact(dest)}, nil
}

// Ref is the brief of digest d's ref: a blob, a root (no lineage), in
// project's scope, under its restriction r.
func Ref(d types.Digest, project types.ProjectID, r types.Restriction) (types.Ref, error) {
	return knowledge.Seal(types.Ref{Type: types.RefTypeBlob, ID: string(d), Scope: types.RefScope{Project: &project},
		Lineage: []string{}, Restrictions: []types.Restriction{r}, ContentDigest: d})
}

// Decode is the brief b holds; b that is no brief (by its schema, every
// field, nothing else) is an error.
func Decode(b []byte) (types.PlanBrief, error) {
	return contract.Decode[types.PlanBrief](record, b)
}

// Prompt is the builder's prompt from brief p: its title and requirement,
// nothing else.
func Prompt(p types.PlanBrief) []byte {
	return []byte("# " + p.Title + "\n\n" + p.Requirement + "\n")
}
