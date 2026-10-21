// SPDX-License-Identifier: Apache-2.0

// The qualification probe: sealed, never shown to a reviewer (P31).
package inventory

import (
	"slices"
	"testing"
)

func TestQualProbeLowStock(t *testing.T) {
	s := New()
	for sku, qty := range map[string]int{"C": 4, "A": 5, "B": 6, "D": 1} {
		if s.Add(sku, qty, 1) != nil {
			t.Fatal("setup")
		}
	}
	if got := s.LowStock(5); !slices.Equal(got, []string{"C", "D"}) {
		t.Errorf("LowStock(5) = %v, want [C D]", got)
	}
	for _, th := range []int{0, -1, -100} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("LowStock(%d) panics: %v", th, r)
				}
			}()
			if got := s.LowStock(th); len(got) != 0 {
				t.Errorf("LowStock(%d) = %v, want none", th, got)
			}
		}()
	}
}
