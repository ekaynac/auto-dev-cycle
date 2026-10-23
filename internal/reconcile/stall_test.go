// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// #226 entry 184: StallOf, the one rule. A task is stalled only when it is
// admitted, launch is active, nothing of it is claimed or held, it is not
// ending, and no decided action applied or met a moved store; its cause is
// the first of StallCauses its outcomes give. Astra #423 H2's shape (every
// node completed, a no_go nobody repairs: no action at all) is stalled on
// "no action decided"; H1's (RecordReview with no reviewer) on "no
// reviewer registered".
func TestStallOf(t *testing.T) {
	admitted := Snapshot{Gates: Gates{Admits: true, Evaluates: true}, Admitted: Versioned{ID: "tsk_x"}}
	with := func(edit func(*Snapshot)) Snapshot {
		s := admitted
		edit(&s)
		return s
	}
	refused := errors.New("authority: refused")
	for name, c := range map[string]struct {
		s        Snapshot
		outcomes []error
		cause    string // "": not stalled
	}{
		"H2's shape: nothing decided":  {admitted, nil, StallNothing},
		"each action changes nothing":  {admitted, []error{errNothing, errNothing}, StallNothing},
		"H1's shape: no reviewer":      {admitted, []error{Stalled{Cause: StallNoReviewer}}, StallNoReviewer},
		"no executor":                  {admitted, []error{errNoExecutor}, StallNoExecutor},
		"no placer":                    {admitted, []error{errNoPlacer}, StallNoExecutor},
		"refused":                      {admitted, []error{refused}, StallRefused},
		"the first cause wins":         {admitted, []error{refused, errNoExecutor, Stalled{Cause: StallNoReviewer}, errNothing}, StallNoReviewer},
		"an unknown cause is no cause": {admitted, []error{Stalled{Cause: "somewhere/else"}}, StallNothing},
		"one action applied":           {admitted, []error{errNothing, nil}, ""},
		"the store moved":              {admitted, []error{fmt.Errorf("x: %w", eventstore.ErrStaleExpectedState)}, ""},
		"a node claimed or held":       {with(func(s *Snapshot) { s.Busy = true }), nil, ""},
		"ending (ending_blocked's)":    {with(func(s *Snapshot) { s.Ending = &Ending{} }), []error{errNothing}, ""},
		"launch inactive":              {with(func(s *Snapshot) { s.Evaluates = false }), nil, ""},
		"no admitted task":             {with(func(s *Snapshot) { s.Admitted = Versioned{} }), nil, ""},
	} {
		cause, stalled := StallOf(c.s, c.outcomes)
		if stalled != (c.cause != "") || cause != c.cause {
			t.Errorf("%s: stalled %v on %q, want %q", name, stalled, cause, c.cause)
		}
	}
	if !errors.Is(Stalled{Cause: StallNoReviewer}, errNothing) || !errors.Is(errNoExecutor, errNothing) {
		t.Error("a Stalled or no executor is not errNothing: it would be logged as a refusal every pass")
	}
}

// The loop keeps its stall for the board and logs it once per distinct
// cause: never a line per pass; a stall that ends is forgotten.
func TestTheLoopLogsAStallOncePerCause(t *testing.T) {
	var log bytes.Buffer
	l := &Loop{Log: &log, Stalls: &Stalls{}}
	s := Snapshot{Gates: Gates{Evaluates: true}, Admitted: Versioned{ID: "tsk_x"}}
	for range 3 {
		l.stall(s, []error{Stalled{Cause: StallNoReviewer}})
	}
	if cause, ok := l.Stalls.Of("tsk_x"); !ok || cause != StallNoReviewer || strings.Count(log.String(), "\n") != 1 {
		t.Fatalf("after three stalled passes: %q %v, log %q", cause, ok, log.String())
	}
	l.stall(s, nil)
	if cause, _ := l.Stalls.Of("tsk_x"); cause != StallNothing || strings.Count(log.String(), "\n") != 2 {
		t.Errorf("a new cause: %q, log %q", cause, log.String())
	}
	l.stall(s, []error{nil})
	if _, ok := l.Stalls.Of("tsk_x"); ok || strings.Count(log.String(), "\n") != 2 {
		t.Errorf("a pass that moved it: stalled %v, log %q", ok, log.String())
	}
	if _, ok := (*Stalls)(nil).Of("tsk_x"); ok {
		t.Error("a nil Stalls tracks a stall")
	}
}
