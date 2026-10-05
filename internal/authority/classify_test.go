// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// profile is a profile with the policy-relevant fields set; the classifier
// reads nothing else.
func profile() types.ProjectProfile {
	std := types.ProjectProfileUsageProfileOverrideValueStandard
	return types.ProjectProfile{
		DataPolicy: types.ProjectProfileDataPolicy{Floor: types.ProjectProfileDataPolicyFloorContracted,
			Routing: types.ProjectProfileDataPolicyRoutingVerified, AllowedDestinations: []types.Slug{"anthropic", "openai"}},
		UsageProfileOverride: &std,
		ProtectedPaths:       []types.Glob{"docs/decisions/**", "CANON.md"},
		ProtectedDocuments:   []types.Glob{"docs/decisions/*.md"},
	}
}

func classes(t *testing.T, current *types.ProjectProfile, next types.ProjectProfile, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	got := ClassifyProfile(current, next)
	if got == nil {
		got = []string{}
	}
	if !slices.Equal(got, want) {
		t.Errorf("classes %v, want %v", got, want)
	}
}

func usage(v types.ProjectProfileUsageProfileOverrideValue) *types.ProjectProfileUsageProfileOverrideValue {
	return &v
}

func TestClassifyProfile(t *testing.T) {
	cur := profile()
	classes(t, &cur, profile()) // nothing changed
	for name, c := range map[string]struct {
		edit func(p *types.ProjectProfile)
		want []string
	}{
		"a wider floor":    {func(p *types.ProjectProfile) { p.DataPolicy.Floor = types.ProjectProfileDataPolicyFloorFreeUnknown }, []string{WidenData}},
		"a narrower floor": {func(p *types.ProjectProfile) { p.DataPolicy.Floor = types.ProjectProfileDataPolicyFloorOwn }, nil},
		"a wider usage profile": {func(p *types.ProjectProfile) {
			p.UsageProfileOverride = usage(types.ProjectProfileUsageProfileOverrideValueOpen)
		}, []string{WidenData}},
		"the installation's usage": {func(p *types.ProjectProfile) { p.UsageProfileOverride = nil }, []string{WidenData}},
		"a narrower usage profile": {func(p *types.ProjectProfile) {
			p.UsageProfileOverride = usage(types.ProjectProfileUsageProfileOverrideValueOnPrem)
		}, nil},
		"an added destination": {func(p *types.ProjectProfile) {
			p.DataPolicy.AllowedDestinations = []types.Slug{"anthropic", "openai", "deepseek"}
		}, []string{WidenDestinations}},
		"a removed destination":    {func(p *types.ProjectProfile) { p.DataPolicy.AllowedDestinations = []types.Slug{"anthropic"} }, nil},
		"destinations emptied":     {func(p *types.ProjectProfile) { p.DataPolicy.AllowedDestinations = []types.Slug{} }, []string{WidenDestinations}},
		"routing loosened":         {func(p *types.ProjectProfile) { p.DataPolicy.Routing = types.ProjectProfileDataPolicyRoutingReportedOk }, []string{WeakenProtection}},
		"a protected path removed": {func(p *types.ProjectProfile) { p.ProtectedPaths = []types.Glob{"CANON.md"} }, []string{WeakenProtection}},
		"a protected path added":   {func(p *types.ProjectProfile) { p.ProtectedPaths = append(p.ProtectedPaths, "deploy/**") }, nil},
		"a protected doc removed":  {func(p *types.ProjectProfile) { p.ProtectedDocuments = []types.Glob{} }, []string{WeakenProtection}},
		"a topology added":         {func(p *types.ProjectProfile) { p.Integration.Topology = topology(t, "compose.yaml") }, []string{ChangeTopology}},
		"several at once": {func(p *types.ProjectProfile) {
			p.DataPolicy.Floor = types.ProjectProfileDataPolicyFloorFreeUnknown
			p.DataPolicy.Routing = types.ProjectProfileDataPolicyRoutingReportedOk
			p.DataPolicy.AllowedDestinations = []types.Slug{"anthropic", "openai", "deepseek"}
		}, []string{WeakenProtection, WidenData, WidenDestinations}},
	} {
		t.Run(name, func(t *testing.T) {
			next := profile()
			c.edit(&next)
			classes(t, &cur, next, c.want...)
		})
	}
	// Topology: any change, either way.
	with := profile()
	with.Integration.Topology = topology(t, "compose.yaml")
	other := profile()
	other.Integration.Topology = topology(t, "stack.yaml")
	classes(t, &with, other, ChangeTopology)
	classes(t, &with, profile(), ChangeTopology)
	classes(t, &with, with)
}

// A first profile is compared with the strictest baseline: anything but the
// narrowest policy is sensitive.
func TestClassifyFirstProfile(t *testing.T) {
	onPrem := types.ProjectProfileUsageProfileOverrideValueOnPrem
	strict := types.ProjectProfile{DataPolicy: types.ProjectProfileDataPolicy{Floor: types.ProjectProfileDataPolicyFloorOwn,
		Routing: types.ProjectProfileDataPolicyRoutingVerified, AllowedDestinations: []types.Slug{}}, UsageProfileOverride: &onPrem,
		ProtectedPaths: []types.Glob{"CANON.md"}}
	classes(t, nil, strict)
	classes(t, nil, profile(), WidenData, WidenDestinations)
	// Unknown values rank widest: never a silent narrowing.
	oddFloor := strict
	oddFloor.DataPolicy.Floor = "somewhere"
	classes(t, nil, oddFloor, WidenData)
	// Security review: routing from reported_ok to an unknown value loosens it.
	loose := profile()
	loose.DataPolicy.Routing = types.ProjectProfileDataPolicyRoutingReportedOk
	oddRouting := loose
	oddRouting.DataPolicy.Routing = "trust_me"
	classes(t, &loose, oddRouting, WeakenProtection)
	classes(t, &loose, profile())
	oddUsage := strict
	oddUsage.UsageProfileOverride = usage("anything")
	classes(t, nil, oddUsage, WidenData, WidenDestinations) // derived destinations widen with it
}

func topology(t *testing.T, file string) *types.ProjectProfileIntegrationTopologyValue {
	t.Helper()
	var v types.ProjectProfileIntegrationTopologyValue
	if err := json.Unmarshal([]byte(`{"files":["`+file+`"],"adopted_revision":"git:cccccccccccccccccccccccccccccccccccccccc","digest":"sha256:`+
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"+`"}`), &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

// Astra #58: inherited usage may be anything, so moving off it widens unless
// to on-prem, and derived destinations widen with the usage profile.
func TestInheritedUsage(t *testing.T) {
	inherit := profile()
	inherit.UsageProfileOverride = nil
	for name, c := range map[string]struct {
		next types.ProjectProfileUsageProfileOverrideValue
		want []string
	}{
		"inherited to standard": {types.ProjectProfileUsageProfileOverrideValueStandard, []string{WidenData}},
		"inherited to open":     {types.ProjectProfileUsageProfileOverrideValueOpen, []string{WidenData}},
		"inherited to on-prem":  {types.ProjectProfileUsageProfileOverrideValueOnPrem, nil},
	} {
		t.Run(name, func(t *testing.T) {
			next := inherit
			next.UsageProfileOverride = usage(c.next)
			classes(t, &inherit, next, c.want...)
		})
	}
	classes(t, &inherit, inherit)
	// Both lists empty (derived): a usage widening widens the destinations.
	derived := profile()
	derived.DataPolicy.AllowedDestinations = []types.Slug{}
	wider := derived
	wider.UsageProfileOverride = usage(types.ProjectProfileUsageProfileOverrideValueOpen)
	classes(t, &derived, wider, WidenData, WidenDestinations)
	narrower := derived
	narrower.UsageProfileOverride = usage(types.ProjectProfileUsageProfileOverrideValueOnPrem)
	classes(t, &derived, narrower)
}
