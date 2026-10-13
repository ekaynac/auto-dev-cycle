// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
)

// ErrAcceptanceInput: a builder's context would carry a task's acceptance
// (I18). The intent is refused, never written with it omitted.
var ErrAcceptanceInput = errors.New("accept: an acceptance ref among a builder's inputs")

// BuilderIntent is the builder's minimal manifest.intent (10; I10, I18):
// call's record (its id, call, consumer, task, attempt, policy, budget,
// rendered digest, destination, routing) with the role builder, and as
// inputs exactly ctx's (graph.Context of the build node): the included
// ones in order, each sized from bytes by identity digest (a missing size
// is an error), and the omitted ones with their reasons. An input that
// is, or derives at any depth from, one of the task's acceptance blobs
// (restricted) refuses the intent.
func BuilderIntent(call types.ManifestIntent, ctx graph.Rendered, bytes map[types.Digest]int64, restricted []types.Digest) (types.ManifestIntent, error) {
	// The restricted closure over every input, at any depth of lineage
	// (as knowledge's restrictedIDs): it grows until nothing changes.
	all := slices.Clone(ctx.Included)
	for _, o := range ctx.Omitted {
		all = append(all, o.Ref)
	}
	hidden := map[string]bool{}
	for _, d := range restricted {
		hidden[string(d)] = true
	}
	for grew := true; grew; {
		grew = false
		for _, r := range all {
			if !hidden[r.ID] && slices.ContainsFunc(append([]string{string(r.ContentDigest)}, r.Lineage...), func(id string) bool { return hidden[id] }) {
				hidden[r.ID], grew = true, true
			}
		}
	}
	in := call
	in.Consumer.Role = "builder"
	in.Inputs, in.Included, in.Omitted = all, []types.ManifestIntentIncludedItem{}, slices.Clone(ctx.Omitted)
	if i := slices.IndexFunc(all, func(r types.Ref) bool { return hidden[r.ID] }); i >= 0 {
		return types.ManifestIntent{}, fmt.Errorf("%w: %s", ErrAcceptanceInput, all[i].ID)
	}
	for i, r := range ctx.Included {
		size, sized := bytes[r.IdentityDigest]
		if !sized {
			return types.ManifestIntent{}, fmt.Errorf("accept: no size for included input %s", r.ID)
		}
		in.Included = append(in.Included, types.ManifestIntentIncludedItem{Ref: r, Order: int64(i), Bytes: size, Purpose: "build input"})
	}
	return in, knowledge.CheckIntent(in)
}
