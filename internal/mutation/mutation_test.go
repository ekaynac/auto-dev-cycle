// SPDX-License-Identifier: Apache-2.0

//go:build !adcmutation

package mutation

import "testing"

// An ordinary build (every release build) can never switch a mutation on,
// whatever the environment says.
func TestOrdinaryBuildsHaveNoMutations(t *testing.T) {
	if Enabled {
		t.Fatal("mutations are enabled in an ordinary build")
	}
	for _, m := range Known {
		t.Setenv("ADC_MUTATION", m)
		if Is(m) {
			t.Fatalf("%s is on in an ordinary build", m)
		}
	}
}
