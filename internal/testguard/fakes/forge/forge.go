// SPDX-License-Identifier: Apache-2.0

// Package forge is the fake forge of docs/design/m0/11 ("Contract-faithful
// fakes"): the remote the merge queue reads its base from and pushes to (13
// "Merge-queue generations").
//
// M0 scope (15 D4, G7b): a head per branch; a push by compare-and-set on
// the expected base, which lands only while the branch still has that head
// (the shape graph.EffectTarget takes: Holds, Apply); and an external base
// move, someone else's push. Each branch has a version that rises with every
// move of its head, so an observation can be ordered (Observe). It logs
// every effect id pushed to a branch, in order. Change requests and
// ambiguous remote outcomes are M1's.
package forge

import (
	"errors"
	"slices"
	"sync"
)

// Forge is safe for concurrent use.
type Forge struct {
	mu       sync.Mutex
	heads    map[string]string
	versions map[string]int64
	pushed   map[string][]string
}

// New is a forge with no branches: every branch's head is "".
func New() *Forge {
	return &Forge{heads: map[string]string{}, versions: map[string]int64{}, pushed: map[string][]string{}}
}

// Observe is branch's head with its version.
func (f *Forge) Observe(branch string) (head string, version int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heads[branch], f.versions[branch]
}

// Holds is branch's head.
func (f *Forge) Holds(branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heads[branch], nil
}

// Apply pushes head to branch under effect's id if branch's head is still
// expected; landed is false, and nothing changes, if it is not.
func (f *Forge) Apply(branch, effect, head, expected string) (landed bool, err error) {
	if branch == "" || effect == "" || head == "" {
		return false, errors.New("forge: a push names its branch, its effect and its head")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.heads[branch] != expected {
		return false, nil
	}
	f.heads[branch] = head
	f.versions[branch]++
	f.pushed[branch] = append(f.pushed[branch], effect)
	return true, nil
}

// Move is an external base move: branch's head becomes head, outside the
// controller.
func (f *Forge) Move(branch, head string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heads[branch] = head
	f.versions[branch]++
}

// Pushed lists the effect ids pushed to branch, in order.
func (f *Forge) Pushed(branch string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pushed[branch])
}
