// SPDX-License-Identifier: Apache-2.0

package api

import (
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
)

// Redact exposes redact to the tests.
var Redact = redact

// BoardCard exposes the board's derivation to the tests, ending_blocked
// none (endingOf reads the store).
func BoardCard(t authority.TaskRecord, nodes []graph.NodeView, now time.Time, ownerRefs bool) types.BoardCard {
	return boardCard(t, nodes, now, ownerRefs, nil)
}
