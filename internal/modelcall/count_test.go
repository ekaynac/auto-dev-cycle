// SPDX-License-Identifier: Apache-2.0

package modelcall_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
)

// The Recorder commits the gateway's count of each request it sent as a
// usage delta, under the call's binding and the gateway's sequence, which
// replay derives: gateway_floor here, since the upstream reports no usage.
func TestTheRecorderCommitsTheGatewaysCount(t *testing.T) {
	w := openWorld(t)
	w.start(rgA)
	g, token := newGateway(t, w, newUpstream(t, nil), modelcall.Recorder{Store: w.s, Actor: actor})
	for range 2 {
		if rec := send(g, token); rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	}
	var ms []types.UsageMeasurement
	w.view(func(tx *eventstore.Tx) (err error) { ms, err = ledger.Measurements(tx, callID); return err })
	seqs := map[int64]bool{}
	for _, m := range ms {
		seqs[m.Sequence] = true
		if m.SourceOfTruth != types.UsageMeasurementSourceOfTruthGatewayFloor || m.Kind != types.UsageMeasurementKindDelta ||
			m.Attempt != attempt || m.Lease != leaseID || m.Tokens.In < 1 {
			t.Fatalf("measurement %+v", m)
		}
	}
	if len(ms) != 2 || !seqs[0] || !seqs[1] {
		t.Fatalf("measurements %+v", ms)
	}
	if o := g.Unbind(token); len(o.Refusals) != 0 {
		t.Fatalf("observed %+v", o)
	}
	if diffs, err := w.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("the counts are not a fold of the log: %v %+v", err, diffs)
	}
}
