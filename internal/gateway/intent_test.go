// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
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

// H1: the intent cache is per token. A call bound under one binding cannot
// be bound under another while bound; two tokens of one binding each reach
// Intents once, and unbinding one leaves the other's entry.
func TestTheIntentIsPerBinding(t *testing.T) {
	in := &intents{}
	up := newUpstream(t, nil)
	g, token := rig(t, up, func(c *Config) { c.Intents = in })
	if _, err := g.Bind(Binding{Lease: leaseID, Attempt: "att_01m4tm99haemss9tvzd438vq8k", Call: callID}); err == nil {
		t.Fatal("a bound call bound under another attempt")
	}
	if _, err := g.Bind(Binding{Lease: leaseID, Attempt: attempt, Call: "msr_01m4tm99haemss9tvzd438vq8j"}); err == nil {
		t.Fatal("a call id of another prefix")
	}
	twin, err := g.Bind(Binding{Lease: leaseID, Attempt: attempt, Call: callID})
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range []string{token, twin, token, twin} {
		if w := call(g, tk, "/chat/completions", good, nil); w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
	if in.n() != 2 {
		t.Fatalf("%d intents for two tokens", in.n())
	}
	g.Unbind(token)
	if w := call(g, twin, "/chat/completions", good, nil); w.Code != http.StatusOK || in.n() != 2 {
		t.Fatalf("after unbinding the other token: %d, %d intents", w.Code, in.n())
	}
}

// flipAttempts is an attempt reader whose answer a test changes between
// requests.
type flipAttempts struct {
	a       types.Attempt
	current bool
	err     error
}

func (f *flipAttempts) Attempt(_ context.Context, id types.AttemptID) (types.Attempt, bool, bool, error) {
	return f.a, f.current, f.a.ID == id, f.err
}

// Astra H1: an active lease does not prove its attempt runs. Every request,
// a token with a cached intent included, is refused once its attempt is
// stopping, not current, of another lease or unit, missing or unreadable.
func TestEveryRequestNeedsTheAttemptRunning(t *testing.T) {
	ok := types.Attempt{ID: attempt, Lease: leaseID, Status: types.AttemptStatusRunning}
	for name, edit := range map[string]func(*flipAttempts){
		"stopping":       func(f *flipAttempts) { f.a.Status = types.AttemptStatusStopping },
		"not current":    func(f *flipAttempts) { f.current = false },
		"another lease":  func(f *flipAttempts) { f.a.Lease = "lea_01m4tm99haemss9tvzd438vq8k" },
		"another unit's": func(f *flipAttempts) { f.a.Unit = "unt_01m3250vz2ezn81cgvbkcn17jd" },
		"missing":        func(f *flipAttempts) { f.a.ID = "" },
		"unreadable":     func(f *flipAttempts) { f.err = errors.New("io") },
	} {
		in, at := &intents{}, &flipAttempts{a: ok, current: true}
		up := newUpstream(t, nil)
		g, token := rig(t, up, func(c *Config) { c.Intents, c.Attempts = in, at })
		if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusOK {
			t.Fatalf("%s: running: %d", name, w.Code)
		}
		edit(at)
		if w := call(g, token, "/chat/completions", good, nil); w.Code == http.StatusOK || len(up.got()) != 1 {
			t.Errorf("%s: a cached token sent: %d, upstream saw %d", name, w.Code, len(up.got()))
		}
		g.Unbind(token)
		fresh, err := g.Bind(Binding{Lease: leaseID, Attempt: attempt, Call: "cal_01m4tm99haemss9tvzd438vq8k"})
		if err != nil {
			t.Fatal(err)
		}
		if w := call(g, fresh, "/chat/completions", good, nil); w.Code == http.StatusOK || len(up.got()) != 1 || in.n() != 1 {
			t.Errorf("%s: a first request sent: %d, upstream saw %d, %d intents", name, w.Code, len(up.got()), in.n())
		}
	}
}
