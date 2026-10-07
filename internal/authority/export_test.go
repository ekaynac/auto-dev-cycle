// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// CountMints counts Enrol's token mints for the rest of the test.
func CountMints(t *testing.T) *int {
	n, orig := new(int), mint
	mint = func(a, by types.ActorID, now time.Time) (identity.Issued, error) {
		*n++
		return orig(a, by, now)
	}
	t.Cleanup(func() { mint = orig })
	return n
}

// SkipStoredCheck makes Enrol's pre-check find no stored key for the rest
// of the test, so a replay reaches the writer with a fresh mint.
func SkipStoredCheck(t *testing.T) {
	orig := hasCommand
	hasCommand = func(*eventstore.Tx, string, string) (bool, error) { return false, nil }
	t.Cleanup(func() { hasCommand = orig })
}
