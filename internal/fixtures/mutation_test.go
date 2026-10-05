// SPDX-License-Identifier: Apache-2.0

//go:build adcmutation

package fixtures

import (
	"context"
	"testing"
)

// Each fixture's named mutation turns it red, and no other fixture's
// mutation is needed to do so (00 "M0 exit check": checked in CI).
func TestEachMutationTurnsItsFixtureRed(t *testing.T) {
	gs, err := Goldens()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range gs {
		t.Run(g.Name, func(t *testing.T) {
			t.Setenv("ADC_MUTATION", g.Mutation)
			r, err := Run(context.Background(), g)
			if err != nil {
				t.Fatal(err)
			}
			if r.Pass {
				t.Fatalf("mutation %s left %s green", g.Mutation, g.Name)
			}
		})
	}
}
