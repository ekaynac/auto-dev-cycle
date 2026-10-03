// SPDX-License-Identifier: Apache-2.0

package inventory

import "fmt"

// FormatCents renders an amount of cents as a decimal string with two
// fraction digits, for example 1999 as "19.99" and -150 as "-1.50".
func FormatCents(cents int) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}
