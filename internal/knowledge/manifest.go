// SPDX-License-Identifier: Apache-2.0

package knowledge

import (
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

// A ContextManifest is three append-only events on one cmf_ id
// (docs/design/m0/10): the intent before egress, a delivery per turn, and
// the finalized record on call end. These functions check what the schemas
// cannot: that cuts are named, that every ref is a checked ref, and that the
// coverage status is derived from evidence.

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

// CheckDelivery checks one per-turn delivery event of the manifest intent:
// a valid document for the same manifest whose delivered refs are checked
// refs, each handle supplied once.
func CheckDelivery(in types.ManifestIntent, d types.ManifestDelivery) error {
	if _, err := contract.Encode("records/manifest-delivery", d); err != nil {
		return fmt.Errorf("knowledge: manifest delivery: %w", err)
	}
	if d.Manifest != in.ID {
		return fmt.Errorf("knowledge: a delivery for manifest %s offered to %s", d.Manifest, in.ID)
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
	}
	return nil
}

// Coverage derives a finalized manifest's delivered refs and status from the
// evidence: the intent, the delivery events, the number of turns (nil when
// unknown), and what is declared-only or unobservable. Status is complete
// only when the number of turns is known, every turn 1..turns has exactly
// one delivery event, and nothing is declared-only or unobservable; unknown
// when the number of turns is unknown; partial otherwise.
func Coverage(in types.ManifestIntent, deliveries []types.ManifestDelivery, turns *int64, declaredOnly, unobservable int) ([]types.Digest, types.ManifestFinalizedCoverageStatus, error) {
	var refs []types.Digest
	seen := map[types.Digest]bool{}
	add := func(d types.Digest) {
		if !seen[d] {
			seen[d] = true
			refs = append(refs, d)
		}
	}
	for _, x := range in.Included {
		add(x.Ref.IdentityDigest)
	}
	evidenced := map[int64]bool{}
	for _, d := range deliveries {
		if err := CheckDelivery(in, d); err != nil {
			return nil, "", err
		}
		if evidenced[d.Turn] {
			return nil, "", fmt.Errorf("knowledge: manifest %s has two delivery events for turn %d", in.ID, d.Turn)
		}
		evidenced[d.Turn] = true
		for _, t := range d.ToolResults {
			add(t.Ref.IdentityDigest)
		}
	}
	slices.Sort(refs)
	switch {
	case turns == nil:
		return refs, types.ManifestFinalizedCoverageStatusUnknown, nil
	case declaredOnly > 0 || unobservable > 0:
		return refs, types.ManifestFinalizedCoverageStatusPartial, nil
	}
	for t := range evidenced {
		if t > *turns {
			return nil, "", fmt.Errorf("knowledge: manifest %s has a delivery for turn %d of %d", in.ID, t, *turns)
		}
	}
	if int64(len(evidenced)) != *turns {
		return refs, types.ManifestFinalizedCoverageStatusPartial, nil
	}
	return refs, types.ManifestFinalizedCoverageStatusComplete, nil
}

// CheckFinalized checks a finalized manifest against its intent and delivery
// events: a valid document for the same manifest whose delivered refs (in
// any order) and status are exactly the derived ones. A status is never
// taken from a declaration.
func CheckFinalized(in types.ManifestIntent, deliveries []types.ManifestDelivery, f types.ManifestFinalized) error {
	if _, err := contract.Encode("records/manifest-finalized", f); err != nil {
		return fmt.Errorf("knowledge: manifest finalized: %w", err)
	}
	if f.Manifest != in.ID {
		return fmt.Errorf("knowledge: a finalized record for manifest %s offered to %s", f.Manifest, in.ID)
	}
	c := f.Coverage
	refs, status, err := Coverage(in, deliveries, c.Turns, len(c.DeclaredOnly), len(c.Unobservable))
	if err != nil {
		return err
	}
	got := slices.Sorted(slices.Values(c.DeliveredRefs))
	if !slices.Equal(got, refs) {
		return errors.New("knowledge: manifest " + string(in.ID) + ": delivered_refs differ from the included inputs and delivered tool results")
	}
	if c.Status != status {
		return fmt.Errorf("knowledge: manifest %s: status %s, the evidence gives %s", in.ID, c.Status, status)
	}
	return nil
}
