// SPDX-License-Identifier: Apache-2.0

// Package capability is M1's activation table (docs/design/m1/00-slice-plan.md
// §1; D52, D58), the one source of truth for gating. Each capability lists
// the invariant-inventory rows it needs enforced
// (docs/design/m1/01-invariant-inventory.md) and what it activates: its
// actions and its effect kinds. A row is enforced when its bit in kept is
// set; the PR that keeps a row sets its bit, and a test ties the bits to
// the inventory's statuses. adcd wires a capability's actions and
// registers its effect kinds only when it is Active; until then its
// actions refuse action_deferred and its effect kinds stay unregistered.
package capability

import "slices"

// The four checks of an invariant (D45).
const (
	Admission   = "admission"
	Fold        = "fold/replay"
	Consumption = "consumption"
	Effect      = "effect"
)

// Row is one inventory row: an invariant ("I8") and one of its checks.
type Row struct{ Invariant, Check string }

// Capability is one row of §1's table.
type Capability struct {
	Name       string
	Checkpoint string // a, b or c
	// ActivatedBy is the slice-plan item that activates it.
	ActivatedBy string
	// Actions are the registered actions it wires; EffectKinds the effect
	// kinds it registers; NodeKinds the node kinds it adds to M1(a)'s
	// (graph.M1Kinds), which adcd records.
	Actions, EffectKinds, NodeKinds []string
	Rows                            []Row
	// Pending are its prerequisites outside the inventory, each one open:
	// a capability with any is not active.
	Pending []string
	// Proven are its prerequisites a deployment proves at adcd's start,
	// not this build's table: open in the table (Active), cleared at the
	// start only by their check (ActiveWith). A capability with any wires
	// no action, effect kind or node kind from the table, whose readers
	// are static (TestTable): only the start's derived value decides it.
	Proven []string
}

// ReviewQualified is review's Proven prerequisite: P31's qualification of
// every registered reviewer route on record (qualify.Reviewers), checked
// by adcd at its start against its release's records.
const ReviewQualified = "P31's qualification on record"

// all is every check of each invariant.
func all(invariants ...string) []Row {
	var rs []Row
	for _, inv := range invariants {
		for _, c := range []string{Admission, Fold, Consumption, Effect} {
			rs = append(rs, Row{inv, c})
		}
	}
	return rs
}

// of is invariant's named checks.
func of(invariant string, checks ...string) []Row {
	var rs []Row
	for _, c := range checks {
		rs = append(rs, Row{invariant, c})
	}
	return rs
}

// Table is §1's table. I15 is required by every capability: intake its
// admission row (the deferred-surface guard), every other one all four.
var Table = []Capability{
	{Name: "intake", Checkpoint: "a", ActivatedBy: "P22a",
		Actions: []string{"project.create", "task.create", "task.cancel"},
		Rows:    slices.Concat(of("I1", Admission, Fold), of("I18", Admission), of("I15", Admission))},
	{Name: "answers", Checkpoint: "a", ActivatedBy: "P6",
		Actions: []string{"input.answer"},
		Rows:    all("I2", "I3", "I4", "I15")},
	{Name: "admission", Checkpoint: "a", ActivatedBy: "P22a",
		Rows: all("I22", "I15")},
	// I6 and I7 are named by their rows in §1 (I6's model_call rows: the
	// inventory keeps model_call and merge in the same four).
	{Name: "launch", Checkpoint: "a", ActivatedBy: "P22a",
		Actions:     []string{"task.raise_rail"}, // a task's count rails (P17a): raised only where attempts run
		EffectKinds: []string{"model_call"},
		Rows:        slices.Concat(of("I5", Admission), all("I6"), of("I7", Consumption), all("I8", "I9", "I10", "I11", "I12", "I16", "I17", "I21", "I22", "I15"))},
	{Name: "review", Checkpoint: "a", ActivatedBy: "P22a",
		Rows:   all("I5", "I7", "I13", "I18", "I15"),
		Proven: []string{ReviewQualified}},
	{Name: "merge-local", Checkpoint: "b", ActivatedBy: "P22b",
		EffectKinds: []string{"merge"}, NodeKinds: []string{"merge"},
		Rows:    slices.Concat(of("I1", Effect), all("I6"), of("I14", Admission, Fold, Consumption), of("I20", Consumption, Effect), all("I15")),
		Pending: []string{"P20's merge half"}},
	{Name: "merge-github", Checkpoint: "b", ActivatedBy: "P22b",
		EffectKinds: []string{"merge"}, NodeKinds: []string{"merge"},
		Rows:    slices.Concat(of("I1", Effect), all("I6", "I14", "I20", "I15")),
		Pending: []string{"P20's merge half, with its GitHub settlement rules and effect_uncertain"}},
	{Name: "repeat", Checkpoint: "c", ActivatedBy: "P29",
		Rows: all("I19", "I22", "I15")},
}

// kept are the required rows that are enforced: kept, or n/a, in the
// inventory. A PR that keeps a required row sets its bit here.
var kept = map[Row]bool{
	{"I1", Admission}: true, {"I1", Fold}: true,
	{"I2", Admission}: true, {"I2", Fold}: true, {"I2", Consumption}: true, {"I2", Effect}: true,
	{"I3", Admission}: true, {"I3", Fold}: true, {"I3", Consumption}: true, {"I3", Effect}: true,
	{"I4", Admission}: true, {"I4", Fold}: true, {"I4", Consumption}: true, {"I4", Effect}: true,
	{"I5", Fold}: true, {"I5", Consumption}: true, {"I5", Effect}: true, // P22a-6d; Admission is launch's activation's (Opus 22a-6d L1)

	{"I6", Admission}: true, {"I6", Fold}: true, {"I6", Consumption}: true, {"I6", Effect}: true, // K1: the model_call half; merge's half (P20's) is gated by merge-local's and merge-github's Pending (Opus #409 L1)
	{"I7", Admission}: true, {"I7", Fold}: true, {"I7", Consumption}: true, {"I7", Effect}: true, // P22a-6d
	{"I8", Admission}: true, {"I8", Fold}: true, {"I8", Consumption}: true, {"I8", Effect}: true, // K1
	{"I9", Admission}: true, {"I9", Fold}: true, {"I9", Consumption}: true, {"I9", Effect}: true, // K1
	{"I10", Admission}: true, {"I10", Fold}: true, {"I10", Consumption}: true, {"I10", Effect}: true, // K2 (launch's kept bits)
	{"I11", Admission}: true, {"I11", Fold}: true, {"I11", Consumption}: true, {"I11", Effect}: true,
	{"I12", Admission}: true, {"I12", Fold}: true, {"I12", Consumption}: true, {"I12", Effect}: true, // K2: Admission for the Go profile
	{"I13", Fold}: true, {"I13", Consumption}: true, {"I13", Effect}: true, // Admission is launch's activation's (Opus 22a-6d L1)
	{"I14", Fold}: true, {"I14", Consumption}: true,
	{"I15", Admission}: true, {"I15", Fold}: true, {"I15", Consumption}: true, {"I15", Effect}: true,
	{"I16", Admission}: true, {"I16", Fold}: true, {"I16", Consumption}: true, {"I16", Effect}: true, // K1
	{"I17", Admission}: true, {"I17", Fold}: true, {"I17", Consumption}: true, {"I17", Effect}: true, // K2
	{"I18", Admission}: true, {"I18", Fold}: true, {"I18", Consumption}: true, {"I18", Effect}: true, // P22a-6d
	{"I21", Consumption}: true,
	{"I22", Admission}:   true, {"I22", Fold}: true, {"I22", Consumption}: true, {"I22", Effect}: true,
}

// Kept reports whether r is enforced.
func Kept(r Row) bool { return kept[r] }

// Missing are c's rows not yet enforced, in order.
func (c Capability) Missing() []Row {
	var out []Row
	for _, r := range c.Rows {
		if !kept[r] {
			out = append(out, r)
		}
	}
	return out
}

// Active: every one of c's rows is enforced and none of its prerequisites
// is pending or to be proven. The build's table: a capability with Proven
// prerequisites is never active here, and no runtime decision reads it
// (ActiveWith).
func (c Capability) Active() bool {
	return len(c.Missing()) == 0 && len(c.Pending) == 0 && len(c.Proven) == 0
}

// ActiveWith is c's activation at adcd's start, derived: checks holds each
// Proven prerequisite's check, nil once it passed. open are the
// prerequisites still open: Pending, then each Proven one unchecked or
// failing, with its check's reason.
func (c Capability) ActiveWith(checks map[string]error) (active bool, open []string) {
	open = append(open, c.Pending...)
	for _, p := range c.Proven {
		switch err, checked := checks[p]; {
		case !checked:
			open = append(open, p+": not checked")
		case err != nil:
			open = append(open, p+": "+err.Error())
		}
	}
	return len(c.Missing()) == 0 && len(open) == 0, open
}

// Of is the capability named name.
func Of(name string) (Capability, bool) {
	i := slices.IndexFunc(Table, func(c Capability) bool { return c.Name == name })
	if i < 0 {
		return Capability{}, false
	}
	return Table[i], true
}

// Gating is the capability that wires action, if any does.
func Gating(action string) (Capability, bool) {
	i := slices.IndexFunc(Table, func(c Capability) bool { return slices.Contains(c.Actions, action) })
	if i < 0 {
		return Capability{}, false
	}
	return Table[i], true
}

// EffectKinds are the effect kinds of the active capabilities, sorted, each once.
func EffectKinds() []string { return active(func(c Capability) []string { return c.EffectKinds }) }

// NodeKinds are the node kinds the active capabilities add, sorted, each once.
func NodeKinds() []string { return active(func(c Capability) []string { return c.NodeKinds }) }

// active is of's union over the active capabilities, sorted, each once.
func active(of func(Capability) []string) []string {
	var out []string
	for _, c := range Table {
		if c.Active() {
			out = append(out, of(c)...)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
