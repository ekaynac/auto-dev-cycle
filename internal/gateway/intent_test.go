// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// F (P12a): the intent precedes egress. When the upstream first sees a
// request, the call's intent is already recorded; it is recorded once per
// binding, with the lease's destination; a request refused before egress
// records none.
func TestTheIntentPrecedesEgress(t *testing.T) {
	in := &intents{}
	var before []int
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		before = append(before, in.n())
		_, _ = w.Write([]byte(`{}`))
	})
	g, token := rig(t, up, func(c *Config) { c.Intents = in })
	if w := call(g, token, "/chat/completions", `{"model":"stub-reviewer-1"}`, nil); w.Code != http.StatusForbidden || in.n() != 0 {
		t.Fatalf("a refused request: %d, %d intents", w.Code, in.n())
	}
	for range 2 {
		if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
	if len(before) != 2 || before[0] != 1 || before[1] != 1 {
		t.Fatalf("intents recorded when the upstream saw each request: %v", before)
	}
	if in.got[0] != (Binding{Lease: leaseID, Attempt: attempt, Call: callID}) || in.dest[0] != "stub" {
		t.Fatalf("intended %+v at %s", in.got[0], in.dest[0])
	}
	// A new binding of another call is intended afresh; after Unbind, the
	// gateway forgets the first.
	g.Unbind(token)
	other, err := g.Bind(Binding{Lease: leaseID, Attempt: attempt, Call: "cal_01m4tm99haemss9tvzd438vq8k"})
	if err != nil {
		t.Fatal(err)
	}
	if w := call(g, other, "/chat/completions", good, nil); w.Code != http.StatusOK || in.n() != 2 {
		t.Fatalf("another call: %d, %d intents", w.Code, in.n())
	}
}

// An intent that is not recorded sends nothing.
func TestNoIntentNoEgress(t *testing.T) {
	up := newUpstream(t, nil)
	g, token := rig(t, up, func(c *Config) { c.Intents = &intents{err: errors.New("disk full")} })
	w := call(g, token, "/chat/completions", good, nil)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "intent_unrecorded") || len(up.got()) != 0 {
		t.Fatalf("%d %s, upstream saw %d", w.Code, w.Body, len(up.got()))
	}
	if strings.Contains(w.Body.String(), "disk full") {
		t.Fatal("the refusal carries the cause")
	}
	if _, err := g.Bind(Binding{Lease: leaseID, Attempt: attempt}); err == nil {
		t.Fatal("a binding with no call")
	}
}
