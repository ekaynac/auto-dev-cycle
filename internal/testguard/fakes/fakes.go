// SPDX-License-Identifier: Apache-2.0

// Package fakes is the fake registry (docs/design/m0/15 L5): every fake in
// the repository, what it is faithful to, and how far M0 builds it. When a
// real implementation is found to differ from its fake, the fake is fixed
// first (11 "Contract-faithful fakes"); this list is where that starts.
// Tests hold it to the repository and to 11's table, row for row.
package fakes

// Fake is one registered fake.
type Fake struct {
	// Name is the fake's row in 11's table.
	Name string
	// Path is the fake's directory from the repository root.
	Path string
	// Built is whether Path exists yet. A fake no M0 fixture consumes yet
	// is registered unbuilt, at the path it will have.
	Built bool
	// M0Scope is how far M0 builds the fake (15 D4): the "M0 scope" cell of
	// its row; it starts with "built" exactly when Built is set.
	M0Scope string
}

// Prefix is where the fakes without another home live; 11's table spells it
// "…/fakes/".
const Prefix = "internal/testguard/fakes/"

// All returns the registry.
func All() []Fake {
	return []Fake{
		{"Fake adapter", "adapters/fake", true, "built: every scenario in `adapters/fake/src/fake.ts`"},
		{"Fake forge", Prefix + "forge", true, "built: a head per branch, a push by compare-and-set on the expected base, and an external base move (G7b)"},
		{"Fake sandbox", Prefix + "sandbox", true, "built: a workspace with exactly its declared facts, and evidence-target restores a test scripts to fail (G8)"},
		{"Fake provider", Prefix + "provider", false, "none here: the 402 mid-call is scripted in `inproc` peers (15 L3); the rest is M2"},
		{"Fake clock", Prefix + "clock", true, "built: a fixed or wall-following time the test sets and moves; no timers"},
		{"Fake executor registry", Prefix + "executors", true, "built: executor id to model family, unregistered is `unknown` (G6a)"},
		{"In-process adapter peer", "internal/adapterhost/session/inproc", true, "built: an in-process adapter peer for the fixture harness (09)"},
		{"Fake scheduler", "internal/capacity/fakesched", true, "built: the deterministic scheduler of 05 \"What M0 delivers\""},
		{"Fake authenticator", "internal/authority/fakeauthn", true, "built: a WebAuthn authenticator for the challenge fixtures (11)"},
		{"Fake owed-answer authority", Prefix + "owedanswers", true, "built: an owed input's answer, granted or revoked by event and read through the transaction (G4b); who may answer is M1's"},
		{"Fake effect target", Prefix + "effecttarget", true, "built: a value held per target, applied by compare-and-set under an effect id and logged in order (G7a)"},
	}
}
