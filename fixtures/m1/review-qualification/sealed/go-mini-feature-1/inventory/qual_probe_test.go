// SPDX-License-Identifier: Apache-2.0

// The qualification probe: sealed, never shown to a reviewer (P31).
package inventory

import (
	"errors"
	"testing"
)

func TestQualProbeTransfer(t *testing.T) {
	src, dst := New(), New()
	if src.Add("A-1", 5, 300) != nil || dst.Add("A-1", 1, 500) != nil || src.Add("B", 2, 10) != nil {
		t.Fatal("setup")
	}
	if err := src.Transfer(dst, " a-1 ", 2); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if src.Qty("A-1") != 3 || dst.Qty("A-1") != 3 || dst.TotalValueCents() != 3*300 {
		t.Errorf("after a transfer: source %d, destination %d worth %d; want 3, 3 at the source's price 300", src.Qty("A-1"), dst.Qty("A-1"), dst.TotalValueCents())
	}
	if err := src.Transfer(dst, "B", 2); err != nil || len(src.SKUs()) != 1 || dst.Qty("B") != 2 {
		t.Errorf("a whole stock: %v, source %v", err, src.SKUs())
	}
	for _, c := range []struct {
		dst  *Store
		sku  string
		qty  int
		want error
	}{{src, "A-1", 1, ErrSameStore}, {dst, "bad sku", 1, ErrInvalidSKU}, {dst, "A-1", 0, ErrInvalidQuantity}, {dst, "Z", 1, ErrUnknownSKU}, {dst, "A-1", 4, ErrInsufficientStock}} {
		if err := src.Transfer(c.dst, c.sku, c.qty); !errors.Is(err, c.want) || src.Qty("A-1") != 3 || dst.Qty("A-1") != 3 {
			t.Errorf("Transfer(%q, %d) = %v, want %v and no change", c.sku, c.qty, err, c.want)
		}
	}
}
