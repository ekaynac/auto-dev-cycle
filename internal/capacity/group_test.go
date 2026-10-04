// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

func group(kind types.ResourceGroupKind, unit types.CapacityUnit, limit *int64, state types.ResourceGroupCapacityModelQuotaState) types.ResourceGroup {
	rule := types.ResourceGroupReleaseRuleOnSettle
	if kind == types.ResourceGroupKindEvidenceTarget {
		rule = types.ResourceGroupReleaseRuleOnRestoreVerified
	}
	return types.ResourceGroup{ID: "rg_01m3250vkwffc979nzjw0g2sf9", Kind: kind, ReleaseRule: rule, Members: []types.ExecutorID{},
		CapacityModel: types.ResourceGroupCapacityModel{Unit: unit, Limit: limit, QuotaState: state}}
}

func n(v int64) *int64 { return &v }

func TestCheckGroup(t *testing.T) {
	known, unknown := types.ResourceGroupCapacityModelQuotaStateKnown, types.ResourceGroupCapacityModelQuotaStateUnknown
	target, broker := types.ResourceGroupKindEvidenceTarget, types.ResourceGroupKindBrokerBalance
	for name, c := range map[string]struct {
		g  types.ResourceGroup
		ok bool
	}{
		"a one-slot target":         {group(target, types.CapacityUnitSlots, n(1), known), true},
		"a two-slot target":         {group(target, types.CapacityUnitSlots, n(2), known), false},
		"a target of unknown size":  {group(target, types.CapacityUnitSlots, nil, unknown), false},
		"a target estimated at one": {group(target, types.CapacityUnitSlots, n(1), types.ResourceGroupCapacityModelQuotaStateEstimated), false},
		"a target in tokens":        {group(target, types.CapacityUnitTokens, n(1), known), false},
		"a broker in usd_micros":    {group(broker, types.CapacityUnitUsdMicros, n(5000000), known), true},
		"a broker in tokens":        {group(broker, types.CapacityUnitTokens, n(5000000), known), false},
		"a quota in tokens":         {group(types.ResourceGroupKindAccountQuota, types.CapacityUnitTokens, nil, unknown), true},
	} {
		// Each case is schema-valid; only the nested rules decide.
		if _, err := contract.Encode("records/resource-group", c.g); err != nil {
			t.Fatalf("%s: fixture: %v", name, err)
		}
		if err := CheckGroup(c.g); (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}
