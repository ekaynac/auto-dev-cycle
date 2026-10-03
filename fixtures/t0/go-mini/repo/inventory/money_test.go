// SPDX-License-Identifier: Apache-2.0

package inventory

import "testing"

func TestFormatCentsPositive(t *testing.T) {
	cases := map[int]string{
		0:      "0.00",
		7:      "0.07",
		1999:   "19.99",
		100000: "1000.00",
	}
	for in, want := range cases {
		if got := FormatCents(in); got != want {
			t.Errorf("FormatCents(%d) = %q, want %q", in, got, want)
		}
	}
}
