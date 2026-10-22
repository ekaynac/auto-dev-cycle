// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"reflect"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// P22a-4 (W14; D59): Decide is a pure function of its snapshot. It admits
// the next task while none is admitted, abandons the admitted task once its
// approval is revoked (#226 entry 100), whatever is runnable, and evaluates
// the admitted task's runnable nodes only when evaluation is allowed; a
// busy store calls for nothing; nothing is admitted while admission is
// off (Opus #312 L4). With evaluation allowed it also matches each
// unplaced build and reopens each node held on the day's USD rail
// (P22a-5c-1), after the evaluation; with it off, neither. Each action
// expects every subject it read at the version it read it.
func TestDecide(t *testing.T) {
	next := Versioned{ID: "tsk_01m3250v9xft08ssr4ygq6g2np", Version: 7}
	plan := Versioned{ID: "nod_01m3250v9xft08ssr4ygq6g2np", Version: 9}
	admitted := Versioned{ID: "tsk_01m3250v9xft08ssr4ygq6g2nq", Version: 11}
	build := Versioned{ID: "nod_01m3250v9xft08ssr4ygq6g2nr", Version: 13}
	held := Held{Node: Versioned{ID: "nod_01m3250v9xft08ssr4ygq6g2ns", Version: 15}, Hold: "hld_01m3250v9xft08ssr4ygq6g2nt"}
	for name, c := range map[string]struct {
		s    Snapshot
		want []Action
	}{
		"nothing to do":                     {Snapshot{}, nil},
		"a task to admit":                   {Snapshot{Next: next, Gates: Gates{Admits: true}}, []Action{Admit{Task: next}}},
		"a task to admit, admission off":    {Snapshot{Next: next}, nil},
		"admitted, nothing runnable":        {Snapshot{Admitted: admitted, Evaluates: true}, nil},
		"admitted, open, evaluation off":    {Snapshot{Admitted: admitted, Open: []Versioned{plan}}, nil},
		"admitted, open":                    {Snapshot{Admitted: admitted, Open: []Versioned{plan}, Evaluates: true}, []Action{Evaluate{Task: types.TaskID(admitted.ID), Goals: []Versioned{plan}}}},
		"admitted, and a next (never both)": {Snapshot{Admitted: admitted, Next: next, Gates: Gates{Admits: true, Evaluates: true}}, nil},
		"admitted, its approval revoked":    {Snapshot{Admitted: admitted, Revoked: true, Open: []Versioned{plan}, Evaluates: true}, []Action{Abandon{Task: admitted}}},
		"a build to place, a day hold": {Snapshot{Admitted: admitted, Open: []Versioned{plan}, Unplaced: []Versioned{build}, DayHeld: []Held{held}, Evaluates: true},
			[]Action{Evaluate{Task: types.TaskID(admitted.ID), Goals: []Versioned{plan}}, Match{Node: build}, ClearRail{Held: held}}},
		"a build to place, nothing open":         {Snapshot{Admitted: admitted, Unplaced: []Versioned{build}, Evaluates: true}, []Action{Match{Node: build}}},
		"a build to place, evaluation off":       {Snapshot{Admitted: admitted, Unplaced: []Versioned{build}, DayHeld: []Held{held}}, nil},
		"a build to place, the approval revoked": {Snapshot{Admitted: admitted, Revoked: true, Unplaced: []Versioned{build}, Evaluates: true}, []Action{Abandon{Task: admitted}}},
		"a plan to record, then a build":         {Snapshot{Admitted: admitted, Plans: []Versioned{plan}, Unplaced: []Versioned{build}, Evaluates: true}, []Action{Plan{Node: plan}, Match{Node: build}}},
		"a plan to record, evaluation off":       {Snapshot{Admitted: admitted, Plans: []Versioned{plan}}, nil},
	} {
		if got := Decide(c.s); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %#v, want %#v", name, got, c.want)
		}
	}
	for a, want := range map[Action][]eventstore.Expectation{
		Admit{Task: next}:       {{Subject: next.ID, Version: 7}},
		Abandon{Task: admitted}: {{Subject: admitted.ID, Version: 11}},
		Match{Node: build}:      {{Subject: build.ID, Version: 13}},
		ClearRail{Held: held}:   {{Subject: held.Node.ID, Version: 15}},
	} {
		if got := a.expected(); !reflect.DeepEqual(got, want) {
			t.Errorf("%T expects %v", a, got)
		}
	}
	if got := (Evaluate{Task: types.TaskID(admitted.ID), Goals: []Versioned{plan}}).expected(); !reflect.DeepEqual(got, []eventstore.Expectation{{Subject: plan.ID, Version: 9}}) {
		t.Errorf("evaluate expects %v", got)
	}
}
