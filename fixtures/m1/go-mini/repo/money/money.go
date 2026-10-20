// SPDX-License-Identifier: Apache-2.0

// Package money holds amounts of money in cents: parsed from text,
// formatted back, and added without overflow.
package money

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// Cents is an amount in hundredths of a unit.
type Cents int64

// ErrSyntax: the text is not an amount.
var ErrSyntax = errors.New("money: not an amount")

// ErrOverflow: the result does not fit in Cents.
var ErrOverflow = errors.New("money: overflow")

// Parse reads an amount: an optional minus sign, digits, and at most two
// decimals ("12.34", "-0.5", "7").
func Parse(s string) (Cents, error) {
	neg := strings.HasPrefix(s, "-")
	whole, frac, dotted := strings.Cut(strings.TrimPrefix(s, "-"), ".")
	if whole == "" || len(frac) > 2 || dotted && frac == "" || !digits(whole) || !digits(frac) {
		return 0, ErrSyntax
	}
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || units > math.MaxInt64/100-1 {
		return 0, ErrOverflow
	}
	for len(frac) < 2 {
		frac += "0"
	}
	cents, _ := strconv.ParseInt(frac, 10, 64)
	c := Cents(units*100 + cents)
	if neg {
		c = -c
	}
	return c, nil
}

func digits(s string) bool {
	return strings.Trim(s, "0123456789") == ""
}

// String formats c with two decimals: "12.34", "-0.50".
func (c Cents) String() string {
	sign, v := "", int64(c)
	if v < 0 {
		sign = "-"
	}
	u := uint64(v)
	if v < 0 {
		u = -u
	}
	frac := strconv.FormatUint(u%100, 10)
	if len(frac) < 2 {
		frac = "0" + frac
	}
	return sign + strconv.FormatUint(u/100, 10) + "." + frac
}

// Add is a+b, or ErrOverflow.
func Add(a, b Cents) (Cents, error) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, ErrOverflow
	}
	return a + b, nil
}
