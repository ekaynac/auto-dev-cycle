// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"errors"
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
)

// ErrAcceptanceInput: a builder's context would carry a task's acceptance
// (I18). The intent is refused, never written with it omitted.
var ErrAcceptanceInput = errors.New("accept: an acceptance ref among a builder's inputs")

// ErrAncestryUnresolved: an input's ancestry is not fully known, so it
// cannot be shown free of acceptance; it is refused as ErrAcceptanceInput.
var ErrAncestryUnresolved = fmt.Errorf("%w: an ancestor is not resolvable", ErrAcceptanceInput)

// maxAncestors bounds one input's ancestry walk.
const maxAncestors = 10_000

// Ancestry resolves a ref id to its recorded lineage, from controller-
// owned records (never from the refs themselves): ok false when the id is
// unknown. A root has an empty lineage.
type Ancestry func(id string) (lineage []string, ok bool)

// BuilderIntent is the builder's minimal manifest.intent (10; I10, I18):
// call's record (its id, call, consumer, task, attempt, policy, budget,
// rendered digest, destination, routing) with the role builder, and as
// inputs exactly ctx's (graph.Context of the build node): the included
// ones in order, each sized from bytes by identity digest (a missing size
// is an error), and the omitted ones with their reasons. An input that
// is, or derives at any depth from, one of the task's acceptance blobs
// (restricted) refuses the intent; so does one with an ancestor ancestry
// cannot resolve. Whether its intermediates are in ctx does not matter.
func BuilderIntent(call types.ManifestIntent, ctx graph.Rendered, bytes map[types.Digest]int64, restricted []types.Digest, ancestry Ancestry) (types.ManifestIntent, error) {
	all := append([]types.Ref{}, ctx.Included...)
	for _, o := range ctx.Omitted {
		all = append(all, o.Ref)
	}
	hidden := map[string]bool{}
	for _, d := range restricted {
		hidden[string(d)] = true
	}
	for _, r := range all {
		if err := clean(r, hidden, ancestry); err != nil {
			return types.ManifestIntent{}, err
		}
	}
	in := call
	in.Consumer.Role = "builder"
	in.Inputs, in.Included, in.Omitted = all, []types.ManifestIntentIncludedItem{}, append([]types.ManifestIntentOmittedItem{}, ctx.Omitted...)
	for i, r := range ctx.Included {
		size, sized := bytes[r.IdentityDigest]
		if !sized {
			return types.ManifestIntent{}, fmt.Errorf("accept: no size for included input %s", r.ID)
		}
		in.Included = append(in.Included, types.ManifestIntentIncludedItem{Ref: r, Order: int64(i), Bytes: size, Purpose: "build input"})
	}
	return in, knowledge.CheckIntent(in)
}

// clean walks r's whole ancestry: r's id and content digest, its lineage,
// and each ancestor's lineage as ancestry resolves it. A restricted id,
// an unresolvable ancestor or a walk past maxAncestors refuses r.
func clean(r types.Ref, hidden map[string]bool, ancestry Ancestry) error {
	walk := append([]string{r.ID, string(r.ContentDigest)}, r.Lineage...)
	seen := map[string]bool{}
	for i := 0; i < len(walk); i++ {
		id := walk[i]
		switch {
		case seen[id]:
			continue
		case hidden[id]:
			return fmt.Errorf("%w: %s", ErrAcceptanceInput, r.ID)
		case len(seen) > maxAncestors:
			return fmt.Errorf("%w: %s has over %d ancestors", ErrAncestryUnresolved, r.ID, maxAncestors)
		}
		seen[id] = true
		if i < 2 {
			continue // r itself: its lineage is r.Lineage
		}
		var parents []string
		ok := ancestry != nil
		if ok {
			parents, ok = ancestry(id)
		}
		if !ok {
			return fmt.Errorf("%w: %s (ancestor %s)", ErrAncestryUnresolved, r.ID, id)
		}
		walk = append(walk, parents...)
	}
	return nil
}
