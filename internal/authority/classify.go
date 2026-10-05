// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"encoding/json"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
)

// Change classes the profile classifier can find (04 "Action registry and
// effective-change classification").
const (
	WidenData         = "policy.widen_data"
	WidenDestinations = "policy.widen_destinations"
	WeakenProtection  = "policy.weaken_protection"
	ChangeTopology    = "integration.change_topology"
)

// floorRank orders the data floors from strictest to widest.
var floorRank = map[types.ProjectProfileDataPolicyFloor]int{
	types.ProjectProfileDataPolicyFloorOwn: 0, types.ProjectProfileDataPolicyFloorContracted: 1, types.ProjectProfileDataPolicyFloorFreeUnknown: 2,
}

// usageMayWiden reports whether moving the usage profile from current to
// next could widen it. Without the installation's profile in hand, no
// override (inheriting it) may be anything: moving to it may widen, and
// moving from it widens unless to the narrowest profile, on-prem. An unknown
// value ranks widest, so a move is never a silent narrowing.
func usageMayWiden(current, next *types.ProjectProfileUsageProfileOverrideValue) bool {
	switch {
	case current == nil && next == nil:
		return false
	case next == nil:
		return !mutation.Is(mutation.InheritedUsageNarrows)
	case current == nil:
		return usageRank(next) > 0
	}
	return usageRank(next) > usageRank(current)
}

// usageRank orders the explicit usage profiles from narrowest to widest.
func usageRank(u *types.ProjectProfileUsageProfileOverrideValue) int {
	switch *u {
	case types.ProjectProfileUsageProfileOverrideValueOnPrem:
		return 0
	case types.ProjectProfileUsageProfileOverrideValueStandard:
		return 1
	case types.ProjectProfileUsageProfileOverrideValueOpen:
		return 2
	}
	return 3 // unknown: widest, so never a silent narrowing
}

// ClassifyProfile is the effective change of adopting next over current
// (nil for a project's first profile, compared with the strictest baseline):
// the sensitive classes it contains, sorted. Authority classifies by this
// change, never by the action's name.
//
//   - a wider data floor, or a usage profile that may be wider (inherited
//     usage may be anything): policy.widen_data;
//   - an added allowed destination, a list emptied, or a derived (empty)
//     list under a usage profile that may widen: policy.widen_destinations;
//   - looser routing (verified < reported_ok < anything unknown), or a
//     protected path or document removed: policy.weaken_protection;
//   - any change to the integration topology: integration.change_topology.
func ClassifyProfile(current *types.ProjectProfile, next types.ProjectProfile) []string {
	base := strictest()
	if current != nil {
		base = *current
	}
	var classes []string
	add := func(c string) {
		if !slices.Contains(classes, c) {
			classes = append(classes, c)
		}
	}
	nd, cd := next.DataPolicy, base.DataPolicy
	usageWidens := usageMayWiden(base.UsageProfileOverride, next.UsageProfileOverride)
	if rank(nd.Floor) > rank(cd.Floor) || usageWidens {
		add(WidenData)
	}
	// An empty list derives the destinations from the usage profile, so a
	// derived list widens when the usage profile may widen, and emptying an
	// explicit list may widen too.
	derived := len(nd.AllowedDestinations) == 0
	if (derived && (len(cd.AllowedDestinations) > 0 || usageWidens)) || !subset(nd.AllowedDestinations, cd.AllowedDestinations) {
		add(WidenDestinations)
	}
	if routingRank(nd.Routing) > routingRank(cd.Routing) ||
		!subset(base.ProtectedPaths, next.ProtectedPaths) || !subset(base.ProtectedDocuments, next.ProtectedDocuments) {
		add(WeakenProtection)
	}
	if !sameJSON(base.Integration.Topology, next.Integration.Topology) {
		add(ChangeTopology)
	}
	slices.Sort(classes)
	return classes
}

// routingRank orders routing from strictest (verified) to loosest; an
// unknown value ranks loosest, so a move to it is never a silent narrowing.
func routingRank(r types.ProjectProfileDataPolicyRouting) int {
	switch r {
	case types.ProjectProfileDataPolicyRoutingVerified:
		return 0
	case types.ProjectProfileDataPolicyRoutingReportedOk:
		return 1
	}
	return 2
}

// rank is a floor's width; an unknown floor ranks widest.
func rank(f types.ProjectProfileDataPolicyFloor) int {
	if r, ok := floorRank[f]; ok {
		return r
	}
	return len(floorRank)
}

// strictest is the baseline a first profile is compared with: the narrowest
// floor, usage profile and routing, no destinations, no topology, and no
// protection to remove.
func strictest() types.ProjectProfile {
	onPrem := types.ProjectProfileUsageProfileOverrideValueOnPrem
	return types.ProjectProfile{
		DataPolicy: types.ProjectProfileDataPolicy{Floor: types.ProjectProfileDataPolicyFloorOwn,
			Routing: types.ProjectProfileDataPolicyRoutingVerified, AllowedDestinations: []types.Slug{}},
		UsageProfileOverride: &onPrem,
	}
}

func subset[T comparable](a, b []T) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

func sameJSON(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}
