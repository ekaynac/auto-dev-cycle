// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"errors"
	"testing"
)

func TestHiddenTransferMovesStockAndPrice(t *testing.T) {
	src, dst := New(), New()
	if err := src.Add("W-1", 5, 120); err != nil {
		t.Fatal(err)
	}
	if err := dst.Add("w-1", 1, 100); err != nil {
		t.Fatal(err)
	}
	if err := src.Transfer(dst, " w-1 ", 3); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if got := src.Qty("W-1"); got != 2 {
		t.Fatalf("source Qty = %d, want 2", got)
	}
	if got := dst.Qty("W-1"); got != 4 {
		t.Fatalf("destination Qty = %d, want 4", got)
	}
	if got := dst.TotalValueCents(); got != 4*120 {
		t.Fatalf("destination value = %d, want %d (source price carried over)", got, 4*120)
	}
}

func TestHiddenTransferAllDropsSourceItem(t *testing.T) {
	src, dst := New(), New()
	if err := src.Add("X", 2, 10); err != nil {
		t.Fatal(err)
	}
	if err := src.Transfer(dst, "X", 2); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if got := src.SKUs(); len(got) != 0 {
		t.Fatalf("source SKUs = %v, want none", got)
	}
	if got := dst.Qty("X"); got != 2 {
		t.Fatalf("destination Qty = %d, want 2", got)
	}
}

func TestHiddenTransferFailureChangesNothing(t *testing.T) {
	src, dst := New(), New()
	if err := src.Add("Y", 2, 10); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		sku  string
		qty  int
		want error
	}{
		{"Y", 3, ErrInsufficientStock},
		{"Z", 1, ErrUnknownSKU},
		{"Y", 0, ErrInvalidQuantity},
		{"bad sku", 1, ErrInvalidSKU},
	}
	for _, c := range cases {
		if err := src.Transfer(dst, c.sku, c.qty); !errors.Is(err, c.want) {
			t.Errorf("Transfer(%q, %d) = %v, want %v", c.sku, c.qty, err, c.want)
		}
	}
	if got := src.Qty("Y"); got != 2 {
		t.Fatalf("source Qty = %d after failed transfers, want 2", got)
	}
	if got := dst.SKUs(); len(got) != 0 {
		t.Fatalf("destination SKUs = %v after failed transfers, want none", got)
	}
}

func TestHiddenTransferToSelfIsRejected(t *testing.T) {
	s := New()
	if err := s.Add("Q", 2, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.Transfer(s, "Q", 1); !errors.Is(err, ErrSameStore) {
		t.Fatalf("Transfer to self = %v, want ErrSameStore", err)
	}
	if got := s.Qty("Q"); got != 2 {
		t.Fatalf("Qty = %d after rejected self transfer, want 2", got)
	}
}
