// SPDX-License-Identifier: Apache-2.0

package inventory

import "testing"

func TestHiddenFormatCentsSigns(t *testing.T) {
	cases := map[int]string{
		-1:      "-0.01",
		-5:      "-0.05",
		-99:     "-0.99",
		-100:    "-1.00",
		-123456: "-1234.56",
		5:       "0.05",
		123456:  "1234.56",
	}
	for in, want := range cases {
		if got := FormatCents(in); got != want {
			t.Errorf("FormatCents(%d) = %q, want %q", in, got, want)
		}
	}
}
