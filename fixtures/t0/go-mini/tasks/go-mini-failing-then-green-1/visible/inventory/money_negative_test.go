// SPDX-License-Identifier: Apache-2.0

package inventory

import "testing"

func TestFormatCentsNegative(t *testing.T) {
	if got, want := FormatCents(-150), "-1.50"; got != want {
		t.Fatalf("FormatCents(-150) = %q, want %q", got, want)
	}
}
