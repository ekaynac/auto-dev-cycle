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
// busy store calls for nothing. Each action expects every subject it read
// at the version it read it.
func TestDecide(t *testing.T) {
	next := Versioned{ID: "tsk_01m3250v9xft08ssr4ygq6g2np", Version: 7}
	plan := Versioned{ID: "nod_01m3250v9xft08ssr4ygq6g2np", Version: 9}
	admitted := Versioned{ID: "tsk_01m3250v9xft08ssr4ygq6g2nq", Version: 11}
	for name, c := range map[string]struct {
		s    Snapshot
		want []Action
	}{
		"nothing to do":                      {Snapshot{}, nil},
		"a task to admit":                    {Snapshot{Next: next}, []Action{Admit{Task: next}}},
		"admitted, nothing runnable":         {Snapshot{Admitted: admitted, Evaluates: true}, nil},
		"admitted, runnable, evaluation off": {Snapshot{Admitted: admitted, Runnable: []Versioned{plan}}, nil},
		"admitted, runnable":                 {Snapshot{Admitted: admitted, Runnable: []Versioned{plan}, Evaluates: true}, []Action{Evaluate{Task: types.TaskID(admitted.ID), Goals: []Versioned{plan}}}},
		"admitted, and a next (never both)":  {Snapshot{Admitted: admitted, Next: next, Evaluates: true}, nil},
		"admitted, its approval revoked":     {Snapshot{Admitted: admitted, Revoked: true, Runnable: []Versioned{plan}, Evaluates: true}, []Action{Abandon{Task: admitted}}},
	} {
		if got := Decide(c.s); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %#v, want %#v", name, got, c.want)
		}
	}
	for a, want := range map[Action][]eventstore.Expectation{
		Admit{Task: next}:       {{Subject: next.ID, Version: 7}},
		Abandon{Task: admitted}: {{Subject: admitted.ID, Version: 11}},
	} {
		if got := a.expected(); !reflect.DeepEqual(got, want) {
			t.Errorf("%T expects %v", a, got)
		}
	}
	if got := (Evaluate{Task: types.TaskID(admitted.ID), Goals: []Versioned{plan}}).expected(); !reflect.DeepEqual(got, []eventstore.Expectation{{Subject: plan.ID, Version: 9}}) {
		t.Errorf("evaluate expects %v", got)
	}
}
