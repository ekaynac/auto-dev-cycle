// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"slices"
	"testing"
)

func TestPushIsExpectedBase(t *testing.T) {
	f := New()
	f.Move("main", "B1")
	if h, err := f.Holds("main"); err != nil || h != "B1" {
		t.Fatalf("head %q (%v)", h, err)
	}
	if ok, err := f.Apply("main", "eff_a", "M1", "B1"); err != nil || !ok {
		t.Fatalf("a push on the expected base: %v, %v", ok, err)
	}
	f.Move("main", "B2") // someone else pushed
	if ok, err := f.Apply("main", "eff_b", "M2", "M1"); err != nil || ok {
		t.Fatalf("a push on a moved base landed: %v, %v", ok, err)
	}
	if h, _ := f.Holds("main"); h != "B2" {
		t.Fatalf("head %q, want B2", h)
	}
	if got := f.Pushed("main"); !slices.Equal(got, []string{"eff_a"}) {
		t.Fatalf("pushed %v", got)
	}
	for _, a := range [][3]string{{"", "eff_a", "M"}, {"main", "", "M"}, {"main", "eff_a", ""}} {
		if _, err := f.Apply(a[0], a[1], a[2], "B2"); err == nil {
			t.Fatalf("a push without a branch, effect or head was accepted: %v", a)
		}
	}
}

func TestVersionRisesWithEveryMove(t *testing.T) {
	f := New()
	if h, v := f.Observe("main"); h != "" || v != 0 {
		t.Fatalf("a fresh branch: %q at %d", h, v)
	}
	f.Move("main", "A")
	f.Move("main", "B")
	if _, err := f.Apply("main", "eff_a", "A", "B"); err != nil {
		t.Fatal(err)
	}
	if h, v := f.Observe("main"); h != "A" || v != 3 {
		t.Fatalf("after A, B and a push of A: %q at %d, want A at 3", h, v)
	}
	if _, err := f.Apply("main", "eff_b", "C", "B"); err != nil {
		t.Fatal(err)
	}
	if _, v := f.Observe("main"); v != 3 {
		t.Fatalf("a push that did not land moved the version to %d", v)
	}
}
