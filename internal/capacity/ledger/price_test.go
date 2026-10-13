// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// The charge comes from the tokens at the declared price, never from the
// adapter-reported cost (#226 entry 62): a hostile upstream reporting
// zero, or any amount, changes nothing.
func TestPriceFromTokensOnly(t *testing.T) {
	p := types.RegistryPrice{InputUsdMicrosPerMtok: 3_000_000, OutputUsdMicrosPerMtok: 15_000_000}
	f := Figure{In: 1000, CacheRead: 500, CacheWrite: 500, Out: 100, CostMicros: 0}
	want := int64((2000*3_000_000 + 100*15_000_000) / 1_000_000) // 6000 + 1500
	for _, cost := range []int64{0, 1, 999_999_999} {
		f.CostMicros = cost
		if got, err := PriceMicros(f, p); err != nil || got != want {
			t.Fatalf("reported cost %d: priced %d (%v), want %d", cost, got, err, want)
		}
	}
	if got, _ := PriceMicros(Figure{In: 1}, p); got != 3 {
		t.Fatalf("one input token priced %d, want 3", got)
	}
	if got, _ := PriceMicros(Figure{In: 1}, types.RegistryPrice{InputUsdMicrosPerMtok: 1}); got != 1 {
		t.Fatalf("a fraction of a micro priced %d, want it rounded up to 1", got)
	}
	if got, _ := PriceTokens(1000, p); got != 15000 {
		t.Fatalf("unmeasured tokens priced %d, want 15000 at the output rate", got)
	}
	if _, err := PriceMicros(Figure{In: maxAmount, Out: maxAmount}, types.RegistryPrice{InputUsdMicrosPerMtok: maxAmount, OutputUsdMicrosPerMtok: maxAmount}); err == nil {
		t.Fatal("an overflowing price was not refused")
	}
	if _, err := PriceMicros(Figure{In: -1}, p); err == nil {
		t.Fatal("a negative figure was priced")
	}
}
