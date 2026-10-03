// SPDX-License-Identifier: Apache-2.0

package sku_test

import (
	"errors"
	"testing"

	"example.com/gomini/inventory"
	"example.com/gomini/sku"
)

func TestHiddenNormalize(t *testing.T) {
	cases := map[string]string{
		"abc":        "ABC",
		"  x-9  ":    "X-9",
		"Z":          "Z",
		"a1-b2-c3":   "A1-B2-C3",
		"\tq-1\n":    "Q-1",
		"0123456789": "0123456789",
	}
	for in, want := range cases {
		got, err := sku.Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q, nil", in, got, err, want)
		}
	}
}

func TestHiddenNormalizeRejects(t *testing.T) {
	for _, in := range []string{"", "   ", "a b", "a_b", "Ü", "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456"} {
		if got, err := sku.Normalize(in); !errors.Is(err, sku.ErrInvalid) || got != "" {
			t.Errorf("Normalize(%q) = %q, %v; want \"\", ErrInvalid", in, got, err)
		}
	}
	if sku.MaxLength != 32 {
		t.Fatalf("MaxLength = %d, want 32", sku.MaxLength)
	}
}

func TestHiddenInventoryUsesSKUPackage(t *testing.T) {
	if inventory.MaxSKULength != sku.MaxLength {
		t.Fatalf("inventory.MaxSKULength = %d, sku.MaxLength = %d", inventory.MaxSKULength, sku.MaxLength)
	}
	s := inventory.New()
	err := s.Add("not valid!", 1, 1)
	if !errors.Is(err, sku.ErrInvalid) {
		t.Fatalf("Add with invalid SKU = %v, want an error wrapping sku.ErrInvalid", err)
	}
	if !errors.Is(err, inventory.ErrInvalidSKU) {
		t.Fatalf("Add with invalid SKU = %v, want it to still match inventory.ErrInvalidSKU", err)
	}
}
