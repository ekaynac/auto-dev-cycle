// SPDX-License-Identifier: Apache-2.0

package knowledge

import (
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

// A ContextManifest is three append-only events on one cmf_ id
// (docs/design/m0/10): the intent before egress, a delivery per turn, and
// the finalized record on call end. These functions check what the schemas
// cannot: that cuts are named, that controller context comes only from the
// declared inputs, and that coverage is derived from evidence. The rules are
// stated in the schemas' descriptions, so another implementation reaches
// the same result.

// CheckIntent checks a manifest intent: a valid document whose refs are all
// checked refs, where every input is named exactly once, either included or
// omitted, nothing else is, and the included positions are 0, 1, … in order.
func CheckIntent(in types.ManifestIntent) error {
	if _, err := contract.Encode("records/manifest-intent", in); err != nil {
		return fmt.Errorf("knowledge: manifest intent: %w", err)
	}
	inputs := map[types.Digest]bool{}
	for _, r := range in.Inputs {
		if err := CheckRef(r); err != nil {
			return fmt.Errorf("knowledge: manifest %s input: %w", in.ID, err)
		}
		if inputs[r.IdentityDigest] {
			return fmt.Errorf("knowledge: manifest %s names input %s twice", in.ID, r.ID)
		}
		inputs[r.IdentityDigest] = true
	}
	named := map[types.Digest]bool{}
	name := func(r types.Ref) error {
		if err := CheckRef(r); err != nil {
			return err
		}
		switch {
		case !inputs[r.IdentityDigest]:
			return fmt.Errorf("%s is not a declared input (controller-supplied context comes only from declared inputs)", r.ID)
		case named[r.IdentityDigest]:
			return fmt.Errorf("%s is both included and omitted, or named twice", r.ID)
		}
		named[r.IdentityDigest] = true
		return nil
	}
	orders := make([]int64, 0, len(in.Included))
	for _, x := range in.Included {
		if err := name(x.Ref); err != nil {
			return fmt.Errorf("knowledge: manifest %s included: %w", in.ID, err)
		}
		orders = append(orders, x.Order)
	}
	for _, x := range in.Omitted {
		if err := name(x.Ref); err != nil {
			return fmt.Errorf("knowledge: manifest %s omitted: %w", in.ID, err)
		}
	}
	if len(named) != len(inputs) {
		return fmt.Errorf("knowledge: manifest %s leaves %d input(s) neither included nor omitted (cuts are named, never silent)", in.ID, len(inputs)-len(named))
	}
	for i, o := range orders {
		if o != int64(i) {
			return fmt.Errorf("knowledge: manifest %s: included positions must be 0, 1, … in order (position %d is %d)", in.ID, i, o)
		}
	}
	return nil
}

// CheckDelivery checks one per-turn delivery event against its (checked)
// intent: a valid document for the same manifest, each handle supplied once,
// and every tool result within the declared inputs: either a declared input,
// or a checked ref derived only from declared inputs (non-empty lineage of
// declared input ids) that keeps every restriction those inputs carry.
func CheckDelivery(in types.ManifestIntent, d types.ManifestDelivery) error {
	if _, err := contract.Encode("records/manifest-delivery", d); err != nil {
		return fmt.Errorf("knowledge: manifest delivery: %w", err)
	}
	if d.Manifest != in.ID {
		return fmt.Errorf("knowledge: a delivery for manifest %s offered to %s", d.Manifest, in.ID)
	}
	byIdentity := map[types.Digest]bool{}
	byID := map[string]types.Ref{}
	for _, r := range in.Inputs {
		byIdentity[r.IdentityDigest] = true
		byID[r.ID] = r
	}
	handles := map[string]bool{}
	for _, t := range d.ToolResults {
		if handles[t.Handle] {
			return fmt.Errorf("knowledge: manifest %s turn %d supplies handle %q twice", in.ID, d.Turn, t.Handle)
		}
		handles[t.Handle] = true
		if err := CheckRef(t.Ref); err != nil {
			return fmt.Errorf("knowledge: manifest %s turn %d: %w", in.ID, d.Turn, err)
		}
		if byIdentity[t.Ref.IdentityDigest] {
			continue
		}
		if err := derivedFrom(t.Ref, byID); err != nil {
			return fmt.Errorf("knowledge: manifest %s turn %d: tool result %s: %w", in.ID, d.Turn, t.Ref.ID, err)
		}
	}
	return nil
}

// derivedFrom: r was derived only from the inputs (its lineage is non-empty
// and names only them) and keeps every restriction they carry.
func derivedFrom(r types.Ref, inputs map[string]types.Ref) error {
	if len(r.Lineage) == 0 {
		return fmt.Errorf("is neither a declared input nor derived from one")
	}
	kept := map[string]bool{}
	for _, x := range r.Restrictions {
		kept[restrictionKey(x)] = true
	}
	for _, id := range r.Lineage {
		src, ok := inputs[id]
		if !ok {
			return fmt.Errorf("is derived from %s, which is not a declared input", id)
		}
		for _, x := range src.Restrictions {
			if !kept[restrictionKey(x)] {
				return fmt.Errorf("drops a restriction of %s (project %s)", id, x.Project)
			}
		}
	}
	return nil
}

func restrictionKey(x types.Restriction) string {
	return fmt.Sprintf("%s|%s|%s|%s|%q", x.Project, x.PolicyDigest, x.Floor, x.Routing, x.AllowedDestinations)
}

// Derived is the coverage the evidence gives.
type Derived struct {
	Delivered, PossiblyDelivered []types.Digest // sorted
	Status                       types.ManifestFinalizedCoverageStatus
}

// Coverage derives a finalized manifest's coverage from the (checked)
// intent, the delivery events and the finalized record's own claims, by the
// rules in schemas/records/manifest-finalized.json. It refuses claims the
// evidence contradicts: a turn outside 1..turns, a delivery when the prompt
// was not sent, a compaction mark or declaration the record leaves out.
func Coverage(in types.ManifestIntent, deliveries []types.ManifestDelivery, c types.ManifestFinalizedCoverage) (Derived, error) {
	inTurns := func(t int64) bool { return c.Turns == nil || (t >= 1 && t <= *c.Turns) }
	type item struct {
		turn int64
		what string
	}
	declared := map[item]bool{}
	for _, x := range c.DeclaredOnly {
		if !inTurns(x.Turn) {
			return Derived{}, fmt.Errorf("knowledge: manifest %s: declared-only turn %d is outside the call's turns", in.ID, x.Turn)
		}
		declared[item{x.Turn, x.What}] = true
	}
	compacted := map[int64]bool{}
	for _, x := range c.Unobservable {
		if x.Turn != nil && !inTurns(*x.Turn) {
			return Derived{}, fmt.Errorf("knowledge: manifest %s: unobservable turn %d is outside the call's turns", in.ID, *x.Turn)
		}
		if x.Turn != nil && x.What == types.ManifestFinalizedCoverageUnobservableItemWhatCompaction {
			compacted[*x.Turn] = true
		}
	}
	if c.Prompt == types.ManifestFinalizedCoveragePromptNotSent && (len(deliveries) > 0 || c.Turns == nil || *c.Turns != 0) {
		return Derived{}, fmt.Errorf("knowledge: manifest %s: a prompt that was not sent has no turns and no deliveries", in.ID)
	}
	var d Derived
	delivered := map[types.Digest]bool{}
	if c.Prompt == types.ManifestFinalizedCoveragePromptDelivered {
		for _, x := range in.Included {
			delivered[x.Ref.IdentityDigest] = true
		}
	}
	if c.Prompt == types.ManifestFinalizedCoveragePromptUncertain {
		for _, x := range in.Included {
			d.PossiblyDelivered = append(d.PossiblyDelivered, x.Ref.IdentityDigest)
		}
	}
	evidenced := map[int64]bool{}
	for _, del := range deliveries {
		if err := CheckDelivery(in, del); err != nil {
			return Derived{}, err
		}
		if !inTurns(del.Turn) {
			return Derived{}, fmt.Errorf("knowledge: manifest %s has a delivery for turn %d outside the call's turns", in.ID, del.Turn)
		}
		if evidenced[del.Turn] {
			return Derived{}, fmt.Errorf("knowledge: manifest %s has two delivery events for turn %d", in.ID, del.Turn)
		}
		evidenced[del.Turn] = true
		for _, t := range del.ToolResults {
			delivered[t.Ref.IdentityDigest] = true
		}
		if del.Compacted && !compacted[del.Turn] {
			return Derived{}, fmt.Errorf("knowledge: manifest %s: the compaction in turn %d is missing from unobservable", in.ID, del.Turn)
		}
		for _, what := range del.Declarations {
			if !declared[item{del.Turn, what}] {
				return Derived{}, fmt.Errorf("knowledge: manifest %s: the declaration %q of turn %d is missing from declared_only", in.ID, what, del.Turn)
			}
		}
	}
	for r := range delivered {
		d.Delivered = append(d.Delivered, r)
	}
	slices.Sort(d.Delivered)
	slices.Sort(d.PossiblyDelivered)
	switch {
	case c.Turns == nil, c.Prompt == types.ManifestFinalizedCoveragePromptUncertain:
		d.Status = types.ManifestFinalizedCoverageStatusUnknown
	case len(c.DeclaredOnly) > 0, len(c.Unobservable) > 0, int64(len(evidenced)) != *c.Turns:
		d.Status = types.ManifestFinalizedCoverageStatusPartial
	default:
		d.Status = types.ManifestFinalizedCoverageStatusComplete
	}
	return d, nil
}

// CheckFinalized checks a finalized manifest against its intent (checked
// here too) and delivery events: a valid document for the same manifest
// whose delivered refs, possibly delivered refs (in any order) and status
// are exactly the derived ones. A status is never taken from a declaration.
func CheckFinalized(in types.ManifestIntent, deliveries []types.ManifestDelivery, f types.ManifestFinalized) error {
	if err := CheckIntent(in); err != nil {
		return err
	}
	if _, err := contract.Encode("records/manifest-finalized", f); err != nil {
		return fmt.Errorf("knowledge: manifest finalized: %w", err)
	}
	if f.Manifest != in.ID {
		return fmt.Errorf("knowledge: a finalized record for manifest %s offered to %s", f.Manifest, in.ID)
	}
	d, err := Coverage(in, deliveries, f.Coverage)
	if err != nil {
		return err
	}
	c := f.Coverage
	if !slices.Equal(slices.Sorted(slices.Values(c.DeliveredRefs)), orNil(d.Delivered)) {
		return fmt.Errorf("knowledge: manifest %s: delivered_refs differ from the evidenced prompt and tool results", in.ID)
	}
	if !slices.Equal(slices.Sorted(slices.Values(c.PossiblyDeliveredRefs)), orNil(d.PossiblyDelivered)) {
		return fmt.Errorf("knowledge: manifest %s: possibly_delivered_refs differ from the uncertain prompt's inputs", in.ID)
	}
	if c.Status != d.Status {
		return fmt.Errorf("knowledge: manifest %s: status %s, the evidence gives %s", in.ID, c.Status, d.Status)
	}
	return nil
}

func orNil(s []types.Digest) []types.Digest {
	if len(s) == 0 {
		return nil
	}
	return s
}
