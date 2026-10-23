// SPDX-License-Identifier: Apache-2.0

// Package holdscan plants the raises #400's hold scan must not pass unseen
// (#402): TestTheHoldScanFailsOnWhatItCannotAttribute loads it.
package holdscan

import (
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
)

// FromParam raises a hold whose signal is its parameter's.
func FromParam(tx *eventstore.Tx, n types.NodeID, signal string) ([]eventstore.Event, error) {
	return graph.Raise(tx, n, types.NodeHold{Hold: "source_invalid_config", SignalInstance: signal, Owner: types.OwnerOperator}, nil)
}

// AfterLiteral sets the signal after the hold's literal.
func AfterLiteral(tx *eventstore.Tx, n types.NodeID, detail string) ([]eventstore.Event, error) {
	h := types.NodeHold{Hold: "source_invalid_config", SignalInstance: "placement: profile p", Owner: types.OwnerOperator}
	h.SignalInstance = detail
	return graph.Raise(tx, n, h, nil)
}

// Attributed raises a hold whose signal is an expression: seen as it is.
func Attributed(tx *eventstore.Tx, n types.NodeID, p types.ProjectID) ([]eventstore.Event, error) {
	signal := "placement: profile " + string(p)
	return graph.Raise(tx, n, types.NodeHold{Hold: "source_invalid_config", SignalInstance: signal, Owner: types.OwnerOperator}, nil)
}

// FromMethod raises a hold whose signal is a method of its parameter's.
func FromMethod(tx *eventstore.Tx, n types.NodeID, x *strings.Builder) ([]eventstore.Event, error) {
	return graph.Raise(tx, n, types.NodeHold{Hold: "source_invalid_config", SignalInstance: x.String(), Owner: types.OwnerOperator}, nil)
}

// Concatenated raises a hold whose signal is its parameter concatenated.
func Concatenated(tx *eventstore.Tx, n types.NodeID, signal string) ([]eventstore.Event, error) {
	return graph.Raise(tx, n, types.NodeHold{Hold: "source_invalid_config", SignalInstance: signal + "", Owner: types.OwnerOperator}, nil)
}
