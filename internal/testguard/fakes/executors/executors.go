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
	"fmt"
	"maps"
	"regexp"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
)

// Unknown is the family of an executor the registry does not hold, the one
// internal/receipt compares.
const Unknown = receipt.UnknownFamily

// FamilyPattern is a receipt's model-family name (schemas/records/receipt.json
// $defs/Name; 06 "Names and media types"). A test keeps them equal.
var FamilyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Registry maps executor ids to model families. It cannot change once built.
type Registry struct{ families map[types.ExecutorID]string }

// New returns a registry of a copy of families. It refuses a family that is
// not a receipt's family name, or is Unknown: a case variant such as
// "Claude" would otherwise count as a family apart from "claude".
func New(families map[types.ExecutorID]string) (Registry, error) {
	for id, f := range families {
		if !FamilyPattern.MatchString(f) || f == Unknown {
			return Registry{}, fmt.Errorf("executors: executor %s has family %q, not a model-family name", id, f)
		}
	}
	return Registry{families: maps.Clone(families)}, nil
}

// Family is the executor's model family, or Unknown.
func (r Registry) Family(id types.ExecutorID) string {
	if f, ok := r.families[id]; ok {
		return f
	}
	return Unknown
}
