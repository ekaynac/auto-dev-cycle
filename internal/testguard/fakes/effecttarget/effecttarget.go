// SPDX-License-Identifier: Apache-2.0

// Package effecttarget is the fake effect target of docs/design/m0/11
// ("Contract-faithful fakes"): a deploy target as an effect operation sees
// it (03 "External effects": deploy reconciles by querying the target's
// running version; 13 [G4]).
//
// M0 scope (15 D4, G7a): each named target holds one value, what the last
// applied effect wanted. An apply is a compare-and-set: it lands only while
// the target still holds the value the caller expects to replace (the
// expected-base shape G7b's merge takes), and applying again is idempotent.
// It logs every effect id applied to each target, in order, so a fixture
// sees each egress. It lives outside the store, so a simulated controller
// restart keeps it.
//
// Not modelled: an apply in flight. A real deploy mid-rollout reads as the
// old value here, as it would through a Holds of one value; a target that
// can report a rollout in transition is M1's (G7a review F6).
package effecttarget

import (
	"errors"
	"slices"
	"sync"
)

// Target is safe for concurrent use.
type Target struct {
	mu    sync.Mutex
	holds map[string]string
	log   map[string][]string
}

// New is an empty target: every name holds "".
func New() *Target { return &Target{holds: map[string]string{}, log: map[string][]string{}} }

// Holds is what target holds now: its running version.
func (t *Target) Holds(target string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.holds[target], nil
}

// Apply makes target hold want, under effect's id, if it still holds
// expected; applied is false, and nothing changes, if it does not.
func (t *Target) Apply(target, effect, want, expected string) (applied bool, err error) {
	if target == "" || effect == "" {
		return false, errors.New("effecttarget: an apply names its target and its effect")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.holds[target] != expected {
		return false, nil
	}
	t.holds[target] = want
	t.log[target] = append(t.log[target], effect)
	return true, nil
}

// Landed: an apply under effect landed on target.
func (t *Target) Landed(target, effect string) (bool, error) {
	return slices.Contains(t.Applied(target), effect), nil
}

// Applied lists the effect ids applied to target, in order.
func (t *Target) Applied(target string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.log[target])
}
