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

// BuilderIntent is the builder's minimal manifest.intent (10; I10, I18):
// call's record (its id, call, consumer, task, attempt, policy, budget,
// rendered digest, destination, routing) with the role builder, and as
// inputs exactly ctx's (graph.Context of the build node): the included
// ones in order, sized from bytes by identity digest, and the omitted ones
// with their reasons. An input that is, or derives from, one of the
// task's acceptance blobs (restricted) refuses the intent.
func BuilderIntent(call types.ManifestIntent, ctx graph.Rendered, bytes map[types.Digest]int64, restricted []types.Digest) (types.ManifestIntent, error) {
	hidden := map[string]bool{}
	for _, d := range restricted {
		hidden[string(d)] = true
	}
	check := func(r types.Ref) error {
		for _, id := range append([]string{r.ID, string(r.ContentDigest)}, r.Lineage...) {
			if hidden[id] {
				return fmt.Errorf("%w: %s", ErrAcceptanceInput, r.ID)
			}
		}
		return nil
	}
	in := call
	in.Consumer.Role = "builder"
	in.Inputs, in.Included, in.Omitted = []types.Ref{}, []types.ManifestIntentIncludedItem{}, []types.ManifestIntentOmittedItem{}
	for i, r := range ctx.Included {
		if err := check(r); err != nil {
			return types.ManifestIntent{}, err
		}
		in.Inputs = append(in.Inputs, r)
		in.Included = append(in.Included, types.ManifestIntentIncludedItem{Ref: r, Order: int64(i), Bytes: bytes[r.IdentityDigest], Purpose: "build input"})
	}
	for _, o := range ctx.Omitted {
		if err := check(o.Ref); err != nil {
			return types.ManifestIntent{}, err
		}
		in.Inputs, in.Omitted = append(in.Inputs, o.Ref), append(in.Omitted, o)
	}
	return in, knowledge.CheckIntent(in)
}
