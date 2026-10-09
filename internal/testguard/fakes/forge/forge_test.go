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
