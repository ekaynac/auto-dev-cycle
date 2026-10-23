// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The receipt and roster holds' signals, one rule per kind beside its
// formatter: each reads back as the value that made it; a near miss is
// refused, and ReceiptWait takes no hold whose signal is not one.
func TestTheReceiptAndRosterSignalsRoundTrip(t *testing.T) {
	m := Missing{Name: "gate", Refusal: &receipt.Refusal{Reason: vocab.ReasonReceiptIncomplete}}
	if name, ok := parseReceiptSignal(receiptSignal(m)); !ok || name != m.Name {
		t.Errorf("%q reads back %q %v", receiptSignal(m), name, ok)
	}
	for _, s := range []string{"", "receipt:", "receipt:gate", "receipt:gate:", "receipt::receipt_incomplete", "receipt:gate:no_such_reason",
		"receipt:a b:receipt_incomplete", "receipt:gate:receipt_incomplete:x", "Receipt:gate:receipt_incomplete"} {
		if name, ok := parseReceiptSignal(s); ok {
			t.Errorf("%q reads as %q", s, name)
		}
		if ReceiptWait(types.NodeHold{Hold: vocab.HoldAwaitingReceipt, SignalInstance: s}) {
			t.Errorf("a receipt wait on %q", s)
		}
	}
	if !ReceiptWait(types.NodeHold{Hold: vocab.HoldAwaitingReceipt, SignalInstance: receiptSignal(m)}) {
		t.Error("the receipt wait is not one")
	}
	for _, c := range [][2]string{{"registry", "exe_01m4tm99haemqt8es2qfbwj8pa"}, {"contributor", "exe_01m4tm99haemqt8es2qfbwj8pa"}, {"contributor", "nod_01m4tm99haemqt8es2qfbwj8pa"}} {
		s := IndependenceSignal(c[0], c[1])
		if of, subject, ok := ParseIndependence(s); !ok || of != c[0] || subject != c[1] {
			t.Errorf("%q reads back %q %q %v", s, of, subject, ok)
		}
	}
	if s := (ErrNotIndependent{Executor: "exe_01m4tm99haemqt8es2qfbwj8pa"}).SignalInstance(); s != "registry:exe_01m4tm99haemqt8es2qfbwj8pa" {
		t.Errorf("the registry signal: %q", s)
	}
	for _, s := range []string{"", "registry:", "registry:e", "registry:nod_01m4tm99haemqt8es2qfbwj8pa", "contributor:x", "member:exe_01m4tm99haemqt8es2qfbwj8pa",
		"registry: exe_01m4tm99haemqt8es2qfbwj8pa", "registry:exe_01m4tm99haemqt8es2qfbwj8pa "} {
		if of, subject, ok := ParseIndependence(s); ok {
			t.Errorf("%q reads as %q %q", s, of, subject)
		}
	}
}
