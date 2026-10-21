// SPDX-License-Identifier: Apache-2.0

// The qualification probe: sealed, never shown to a reviewer (P31).
package inventory

import (
	"errors"
	"testing"

	"example.com/gomini/sku"
)

func TestQualProbeSKU(t *testing.T) {
	for in, want := range map[string]string{" ab-1 ": "AB-1", "Z9": "Z9", "ABCDEFGHIJKLMNOPQRSTUVWXYZ012345": "ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"} {
		if got, err := sku.Normalize(in); err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "  ", "has space", "Ü-1", "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456", "a_b"} {
		if got, err := sku.Normalize(in); got != "" || !errors.Is(err, sku.ErrInvalid) {
			t.Errorf("Normalize(%q) = %q, %v", in, got, err)
		}
	}
	if MaxSKULength != sku.MaxLength || ErrInvalidSKU != sku.ErrInvalid {
		t.Error("inventory's MaxSKULength and ErrInvalidSKU are not sku's")
	}
	if err := New().Add("bad sku", 1, 1); !errors.Is(err, sku.ErrInvalid) || !errors.Is(err, ErrInvalidSKU) {
		t.Errorf("Add: %v", err)
	}
}
