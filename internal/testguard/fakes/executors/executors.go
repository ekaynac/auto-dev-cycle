// SPDX-License-Identifier: Apache-2.0

// Package executors is a fake of the controller's executor registry
// (docs/design/m0/13 "The reviewer roster is part of a review's identity"):
// the model family of each executor, from which a roster's families are
// derived and never asserted by a caller.
//
// M0 scope (15 D4, G6a): a fixed registry the test builds once, looked up by
// executor id. An unregistered executor's family is unknown, which never
// counts as independent (06 rule 3). Executor availability and placement
// arrive with the real registry in M1.
package executors

import (
	"maps"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// Unknown is the family of an executor the registry does not hold, as
// internal/receipt compares it.
const Unknown = "unknown"

// Registry maps executor ids to model families. It cannot change once built.
type Registry struct{ families map[types.ExecutorID]string }

// New returns a registry of a copy of families.
func New(families map[types.ExecutorID]string) Registry {
	return Registry{families: maps.Clone(families)}
}

// Family is the executor's model family, or Unknown.
func (r Registry) Family(id types.ExecutorID) string {
	if f, ok := r.families[id]; ok && f != "" {
		return f
	}
	return Unknown
}
