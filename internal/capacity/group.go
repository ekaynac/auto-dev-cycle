// SPDX-License-Identifier: Apache-2.0

// Package capacity holds the resource-group rules (docs/design/m0/05) that
// the record schema cannot express: they cross into the nested capacity
// model.
package capacity

import (
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// CheckGroup refuses a schema-valid resource group that breaks a nested
// rule: a broker balance is counted in usd_micros, and an evidence target
// has a known capacity of exactly one slot, so it is used exclusively and
// nothing runs on an unrestored target (12, 13).
func CheckGroup(g types.ResourceGroup) error {
	m := g.CapacityModel
	switch g.Kind {
	case types.ResourceGroupKindBrokerBalance:
		if m.Unit != types.CapacityUnitUsdMicros {
			return fmt.Errorf("capacity: broker balance %s is counted in %s, not usd_micros", g.ID, m.Unit)
		}
	case types.ResourceGroupKindEvidenceTarget:
		if m.Unit != types.CapacityUnitSlots || m.Limit == nil || *m.Limit != 1 || m.QuotaState != types.ResourceGroupCapacityModelQuotaStateKnown {
			return fmt.Errorf("capacity: evidence target %s must have a known capacity of exactly one slot", g.ID)
		}
	}
	return nil
}
