// SPDX-License-Identifier: Apache-2.0

// Package inventory keeps stock levels and unit prices for a small shop.
package inventory

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// MaxSKULength is the longest SKU accepted after normalisation.
const MaxSKULength = 32

var (
	// ErrInvalidSKU is returned for an empty, too long or malformed SKU.
	ErrInvalidSKU = errors.New("inventory: invalid SKU")
	// ErrInvalidQuantity is returned for a quantity that is not positive.
	ErrInvalidQuantity = errors.New("inventory: quantity must be positive")
	// ErrInvalidPrice is returned for a negative unit price.
	ErrInvalidPrice = errors.New("inventory: price must not be negative")
	// ErrUnknownSKU is returned when the SKU is not stocked.
	ErrUnknownSKU = errors.New("inventory: unknown SKU")
	// ErrInsufficientStock is returned when removing more than is stocked.
	ErrInsufficientStock = errors.New("inventory: insufficient stock")
)

// Item is one stocked SKU.
type Item struct {
	SKU        string
	Qty        int
	PriceCents int
}

// Store holds items keyed by normalised SKU. The zero value is not usable; call New.
type Store struct {
	items map[string]Item
}

// New returns an empty store.
func New() *Store {
	return &Store{items: make(map[string]Item)}
}

// normalizeSKU trims surrounding spaces and upper-cases the SKU, then checks
// that it is 1..MaxSKULength characters of A-Z, 0-9 and '-'.
func normalizeSKU(raw string) (string, error) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if s == "" || len(s) > MaxSKULength {
		return "", fmt.Errorf("%w: %q", ErrInvalidSKU, raw)
	}
	for _, r := range s {
		ok := (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			return "", fmt.Errorf("%w: %q", ErrInvalidSKU, raw)
		}
	}
	return s, nil
}

// Add stocks qty units of sku at priceCents each. Adding an SKU that is
// already stocked increases its quantity and replaces its price.
func (s *Store) Add(sku string, qty, priceCents int) error {
	key, err := normalizeSKU(sku)
	if err != nil {
		return err
	}
	if qty <= 0 {
		return ErrInvalidQuantity
	}
	if priceCents < 0 {
		return ErrInvalidPrice
	}
	it := s.items[key]
	s.items[key] = Item{SKU: key, Qty: it.Qty + qty, PriceCents: priceCents}
	return nil
}

// Remove takes qty units of sku out of stock. An item whose quantity reaches
// zero is dropped from the store.
func (s *Store) Remove(sku string, qty int) error {
	key, err := normalizeSKU(sku)
	if err != nil {
		return err
	}
	if qty <= 0 {
		return ErrInvalidQuantity
	}
	it, ok := s.items[key]
	if !ok {
		return ErrUnknownSKU
	}
	if qty > it.Qty {
		return ErrInsufficientStock
	}
	if qty == it.Qty {
		delete(s.items, key)
		return nil
	}
	it.Qty -= qty
	s.items[key] = it
	return nil
}

// Qty returns the stocked quantity of sku, or 0 if it is unknown or invalid.
func (s *Store) Qty(sku string) int {
	key, err := normalizeSKU(sku)
	if err != nil {
		return 0
	}
	return s.items[key].Qty
}

// SKUs returns every stocked SKU in ascending order.
func (s *Store) SKUs() []string {
	out := make([]string, 0, len(s.items))
	for k := range s.items {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TotalValueCents returns the sum of quantity times unit price over all items.
func (s *Store) TotalValueCents() int {
	total := 0
	for _, it := range s.items {
		total += it.Qty * it.PriceCents
	}
	return total
}

// LowStock returns, in ascending order, the SKUs whose quantity is strictly
// below threshold.
func (s *Store) LowStock(threshold int) []string {
	out := []string{}
	for k, it := range s.items {
		if it.Qty <= threshold {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
