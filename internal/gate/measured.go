// SPDX-License-Identifier: Apache-2.0

package gate

// What a gate run measured its result against (P22a-6a′): the measured
// baseline's digest and base tree, and the platform image, recorded by the
// controller in the command that records the gate's result
// (gate.measured/v1; the fold admits it in no later command). A
// consumer's gate requirement is derived from it (the receipt's demand key
// from the baseline's digest, its instrument and environment from the
// image), never read off the receipt the requirement checks.

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// The measurement's event kind and projection.
const (
	Measured           = "gate.measured/v1"
	MeasuredProjection = "gate_measured" // gate result id → its measurement
	ResultsProjection  = "gate_results"  // gate result id → the command that recorded it
	measuredSchema     = "records/gate-measured"
)

// Kinds are the measurement's events.
func Kinds() eventstore.Kinds { return eventstore.Kinds{Measured: {Schema: measuredSchema}} }

// RecordMeasured is the event recording that result, gate node's, was measured
// against b in img; its subject the node.
func RecordMeasured(node types.NodeID, result types.ResultID, b Baseline, img sandbox.Image) (eventstore.Event, error) {
	d, err := b.Digest()
	if err != nil {
		return eventstore.Event{}, err
	}
	raw, err := json.Marshal(types.GateMeasured{Node: node, Result: result, BaselineDigest: d, BaseTree: types.GitID("git:" + b.Base),
		Image: types.GateMeasuredImage{Pinned: img.Pinned, ID: img.ID, Os: img.OS, Arch: img.Arch}})
	if err != nil {
		return eventstore.Event{}, err
	}
	id, err := ids.New("evt")
	return eventstore.Event{ID: id, Kind: Measured, Subject: string(node), Payload: raw}, err
}

// MeasuredProjector folds the measurements: each under its gate node, of a
// gate_result that node recorded in the same command, once (Opus 22a-6a′
// L3). It indexes each gate_result's recording command (ResultsProjection)
// to tell.
type MeasuredProjector struct{}

// Project folds evs' measurements.
func (MeasuredProjector) Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		if ev.Kind == graph.ResultRecorded {
			if err := indexResult(tx, ev); err != nil {
				return err
			}
			continue
		}
		if ev.Kind != Measured {
			continue
		}
		var m types.GateMeasured
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			return err
		}
		raw, ok, err := tx.Get(graph.ResultProjection, string(m.Result))
		var rr types.ResultRecorded
		if err == nil && ok {
			err = json.Unmarshal(raw, &rr)
		}
		if err != nil {
			return err
		}
		v, verr := graph.ViewOf(tx, m.Node)
		switch {
		case verr != nil:
			return verr
		case ev.Subject != string(m.Node) || !ok || rr.Node != m.Node || v.Node.Kind != types.NodeKindGate || rr.Result.Kind != types.ResultKindGateResult:
			return errors.New("gate: a measurement is under its gate node, of a gate result that node recorded")
		}
		if _, dup, err := tx.Get(MeasuredProjection, string(m.Result)); err != nil || dup {
			return errors.Join(err, fmt.Errorf("gate: result %s's measurement is recorded", m.Result))
		}
		var by string
		raw, ok, err = tx.Get(ResultsProjection, string(m.Result))
		if err == nil && ok {
			err = json.Unmarshal(raw, &by)
		}
		if err != nil || !ok || ev.Command == nil || *ev.Command != by {
			return errors.Join(err, fmt.Errorf("gate: result %s's measurement is recorded in the command that records it, never later", m.Result))
		}
		if err := tx.Put(MeasuredProjection, string(m.Result), ev.Payload); err != nil {
			return err
		}
	}
	return nil
}

// indexResult indexes ev, a result.recorded, by its command when it
// records a gate_result.
func indexResult(tx *eventstore.Tx, ev eventstore.Event) error {
	var rr types.ResultRecorded
	if err := json.Unmarshal(ev.Payload, &rr); err != nil || rr.Result.Kind != types.ResultKindGateResult {
		return err
	}
	if ev.Command == nil {
		return fmt.Errorf("gate: result %s is recorded by no command", rr.Result.ID)
	}
	raw, err := json.Marshal(*ev.Command)
	if err != nil {
		return err
	}
	return tx.Put(ResultsProjection, string(rr.Result.ID), raw)
}

// MeasurementOf is result's measurement, if recorded.
func MeasurementOf(tx *eventstore.Tx, result types.ResultID) (types.GateMeasured, bool, error) {
	raw, ok, err := tx.Get(MeasuredProjection, string(result))
	var m types.GateMeasured
	if err == nil && ok {
		err = json.Unmarshal(raw, &m)
	}
	return m, ok, err
}

// RequirementFor is the gate requirement of a gate receipt serving tree
// under g, the project's gate, and m, a measurement: the instrument
// (Contract) over g in m's image, the tree subject in m's environment, and
// the demand m's baseline digest derives.
func RequirementFor(g types.ProjectProfileGate, tree string, m types.GateMeasured, s Subject) (receipt.Requirement, error) {
	img := sandbox.Image{Pinned: m.Image.Pinned, ID: m.Image.ID, OS: m.Image.Os, Arch: m.Image.Arch}
	c, err := Contract(g, img)
	if err != nil {
		return receipt.Requirement{}, err
	}
	sub, err := subjectAt(tree, img, s, demandOf(m.BaselineDigest))
	return receipt.Requirement{Kind: types.ReceiptKindGate, Instrument: types.InstrumentRef{Name: c.Name, Version: c.Version, Digest: c.Digest},
		Subject: sub}, err
}
