// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"reflect"
	"testing"
)

func TestHiddenLowStockExcludesItemsAtThreshold(t *testing.T) {
	s := New()
	for sku, qty := range map[string]int{"A": 4, "B": 5, "C": 6} {
		if err := s.Add(sku, qty, 1); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := s.LowStock(5), []string{"A"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("LowStock(5) = %v, want %v", got, want)
	}
	if got, want := s.LowStock(7), []string{"A", "B", "C"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("LowStock(7) = %v, want %v", got, want)
	}
	if got := s.LowStock(4); len(got) != 0 {
		t.Fatalf("LowStock(4) = %v, want none", got)
	}
}

func TestHiddenLowStockAfterRemove(t *testing.T) {
	s := New()
	if err := s.Add("A", 3, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("A", 1); err != nil {
		t.Fatal(err)
	}
	if got := s.LowStock(2); len(got) != 0 {
		t.Fatalf("LowStock(2) with A at 2 = %v, want none", got)
	}
	if got, want := s.LowStock(3), []string{"A"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("LowStock(3) = %v, want %v", got, want)
	}
}
