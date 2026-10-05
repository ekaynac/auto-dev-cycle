// SPDX-License-Identifier: Apache-2.0

// Package mutation holds the named mutations that the M0 contract fixtures
// must turn red (docs/design/m0/11 "M0 exercises real validators"; 00 "M0
// exit check"). A mutation site reads `if mutation.Is(name) { … }`. In every
// ordinary build Is is a constant false, so the sites compile away; only a
// build with the adcmutation tag (make fixtures-contract) can switch one on,
// through ADC_MUTATION.
package mutation

// The named mutations, one per contract fixture that targets it.
const (
	// IdempotencyIgnoresInput makes the writer replay a stored result for a
	// reused key whatever the new input is.
	IdempotencyIgnoresInput = "eventstore.idempotency-ignores-input"
	// ExpectedStateUnchecked makes the writer skip the expected-state check.
	ExpectedStateUnchecked = "eventstore.expected-state-unchecked"
	// LockNotExclusive makes the state directory lock ignore a failed
	// exclusive flock, so a second controller could start.
	LockNotExclusive = "eventstore.lock-not-exclusive"
)

// Known lists every named mutation; a fixture golden may name only these.
var Known = []string{IdempotencyIgnoresInput, ExpectedStateUnchecked, LockNotExclusive}
