// SPDX-License-Identifier: Apache-2.0

// The qualification probe: sealed, never shown to a reviewer (P31).
package inventory

import "testing"

func TestQualProbeFormatCents(t *testing.T) {
	for in, want := range map[int]string{-1: "-0.01", -50: "-0.50", -99: "-0.99", -100: "-1.00", -150: "-1.50", -100000: "-1000.00", 0: "0.00", 7: "0.07", 1999: "19.99"} {
		if got := FormatCents(in); got != want {
			t.Errorf("FormatCents(%d) = %q, want %q", in, got, want)
		}
	}
}
