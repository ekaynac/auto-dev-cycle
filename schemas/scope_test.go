// SPDX-License-Identifier: Apache-2.0

package schemas_test

import "testing"

// proposal.withdraw's scope is the proposal alone: its project and object
// are derived from it server side.
func TestWithdrawScope(t *testing.T) {
	const schema, id = "api/scope/proposal-withdraw", "prp_01m3250vkwffc979nzjw0g2sf9"
	if err := valid(t, schema, map[string]any{"proposal": id}); err != nil {
		t.Errorf("a proposal: %v", err)
	}
	for name, d := range map[string]map[string]any{
		"empty":         {},
		"not a prp id":  {"proposal": "grt_01m3250vkwffc979nzjw0g2sf9"},
		"another field": {"proposal": id, "project": "prj_01m3250vkwffc979nzjw0g2sf9"},
	} {
		if valid(t, schema, d) == nil {
			t.Errorf("%s was valid", name)
		}
	}
}
