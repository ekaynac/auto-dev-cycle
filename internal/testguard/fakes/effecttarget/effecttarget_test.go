// SPDX-License-Identifier: Apache-2.0

package effecttarget

import (
	"slices"
	"testing"
)

func TestApplyIsCompareAndSet(t *testing.T) {
	tg := New()
	if h, err := tg.Holds("prod"); err != nil || h != "" {
		t.Fatalf("a fresh target holds %q (%v)", h, err)
	}
	for _, a := range [][3]string{{"eff_a", "A", ""}, {"eff_b", "B", "A"}, {"eff_a", "A", "B"}} {
		if ok, err := tg.Apply("prod", a[0], a[1], a[2]); err != nil || !ok {
			t.Fatalf("apply %v: %v, %v", a, ok, err)
		}
	}
	if ok, err := tg.Apply("prod", "eff_c", "C", "B"); err != nil || ok {
		t.Fatalf("an apply expecting a stale value landed: %v, %v", ok, err)
	}
	if h, _ := tg.Holds("prod"); h != "A" {
		t.Fatalf("holds %q, want A", h)
	}
	if got := tg.Applied("prod"); !slices.Equal(got, []string{"eff_a", "eff_b", "eff_a"}) {
		t.Fatalf("applied %v", got)
	}
	if got := tg.Applied("other"); len(got) != 0 {
		t.Fatalf("another target applied %v", got)
	}
	for _, a := range [][2]string{{"", "eff_a"}, {"prod", ""}} {
		if _, err := tg.Apply(a[0], a[1], "A", "A"); err == nil {
			t.Fatalf("an apply without a target or an effect was accepted: %v", a)
		}
	}
}
