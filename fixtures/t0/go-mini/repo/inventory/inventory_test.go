// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"errors"
	"reflect"
	"testing"
)

func TestAddNormalisesSKUAndAccumulates(t *testing.T) {
	s := New()
	if err := s.Add("  ab-1 ", 3, 250); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Add("AB-1", 2, 300); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := s.Qty("ab-1"); got != 5 {
		t.Fatalf("Qty = %d, want 5", got)
	}
	if got := s.TotalValueCents(); got != 5*300 {
		t.Fatalf("TotalValueCents = %d, want %d", got, 5*300)
	}
}

func TestAddRejectsBadInput(t *testing.T) {
	s := New()
	cases := []struct {
		sku   string
		qty   int
		price int
		want  error
	}{
		{"", 1, 1, ErrInvalidSKU},
		{"has space", 1, 1, ErrInvalidSKU},
		{"Ü-1", 1, 1, ErrInvalidSKU},
		{"ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456", 1, 1, ErrInvalidSKU},
		{"OK", 0, 1, ErrInvalidQuantity},
		{"OK", 1, -1, ErrInvalidPrice},
	}
	for _, c := range cases {
		if err := s.Add(c.sku, c.qty, c.price); !errors.Is(err, c.want) {
			t.Errorf("Add(%q, %d, %d) = %v, want %v", c.sku, c.qty, c.price, err, c.want)
		}
	}
	if len(s.SKUs()) != 0 {
		t.Fatalf("store changed after rejected adds: %v", s.SKUs())
	}
}

func TestRemove(t *testing.T) {
	s := New()
	if err := s.Add("A", 4, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("a", 1); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := s.Qty("A"); got != 3 {
		t.Fatalf("Qty = %d, want 3", got)
	}
	if err := s.Remove("A", 9); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("Remove too many = %v, want ErrInsufficientStock", err)
	}
	if err := s.Remove("B", 1); !errors.Is(err, ErrUnknownSKU) {
		t.Fatalf("Remove unknown = %v, want ErrUnknownSKU", err)
	}
	if err := s.Remove("A", 3); err != nil {
		t.Fatalf("Remove all: %v", err)
	}
	if got := s.SKUs(); len(got) != 0 {
		t.Fatalf("SKUs after removing all = %v, want none", got)
	}
}

func TestLowStock(t *testing.T) {
	s := New()
	for sku, qty := range map[string]int{"C": 1, "A": 2, "B": 10, "D": 50} {
		if err := s.Add(sku, qty, 1); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := s.LowStock(5), []string{"A", "C"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("LowStock(5) = %v, want %v", got, want)
	}
	if got := s.LowStock(0); len(got) != 0 {
		t.Fatalf("LowStock(0) = %v, want none", got)
	}
}
