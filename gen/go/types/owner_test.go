// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

// Astra #28 round 2: only exactly the vocabulary package owns the reason
// check; the same suffix elsewhere does not.
func TestOwnsReasonCheck(t *testing.T) {
	for name, want := range map[string]bool{
		"github.com/ekaynac/auto-dev-cycle/internal/vocab.init.0":                 true,
		"github.com/ekaynac/auto-dev-cycle/internal/vocab.init.0.func1":           true,
		"github.com/ekaynac/auto-dev-cycle/internal/vocab.(*Registry).Check":      true,
		"github.com/ekaynac/auto-dev-cycle/tools/hookprobe/internal/vocab.init.0": false,
		"github.com/ekaynac/auto-dev-cycle/internal/vocab/sub.init.0":             false,
		"example.com/other/internal/vocab.init.0":                                 false,
		"github.com/ekaynac/auto-dev-cycle/internal/vocabulary.init.0":            false,
		"main.main": false,
	} {
		if got := ownsReasonCheck(name); got != want {
			t.Errorf("ownsReasonCheck(%q) = %v, want %v", name, got, want)
		}
	}
}
