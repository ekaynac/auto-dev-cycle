// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

// `adc dev capabilities --json` (P22a-2, W7) prints this build's table:
// intake, answers and admission active (P22a-3); launch inactive, its
// model_call kind and its unkept rows named; review waiting on P31; merge
// never active in (a).
func TestDevCapabilities(t *testing.T) {
	var out, errs bytes.Buffer
	if code := Main(context.Background(), []string{"dev", "capabilities", "--json"}, &out, &errs, nil); code != entry.ExitOK {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	var doc struct{ Capabilities []capabilityView }
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	by := map[string]capabilityView{}
	var names []string
	for _, c := range doc.Capabilities {
		by[c.Name], names = c, append(names, c.Name)
	}
	if !slices.Equal(names, []string{"intake", "answers", "admission", "launch", "review", "merge-local", "merge-github", "repeat"}) {
		t.Fatalf("capabilities: %v", names)
	}
	in, launch, review := by["intake"], by["launch"], by["review"]
	if !in.Active || !by["answers"].Active || !by["admission"].Active || !slices.Equal(in.Actions, []string{"project.create", "task.create", "task.cancel"}) {
		t.Errorf("intake: %+v", in)
	}
	unkept := slices.ContainsFunc(launch.Rows, func(r capabilityRow) bool { return !r.Kept })
	if !launch.Active || !slices.Equal(launch.EffectKinds, []string{"model_call"}) || unkept || !slices.Equal(launch.Actions, []string{"task.raise_rail"}) {
		t.Errorf("launch: %+v", launch)
	}
	if review.Active || len(review.Pending) != 1 || !strings.HasPrefix(review.Pending[0], "P31's qualification on record (checked by adcd at start") ||
		by["merge-local"].Active || by["merge-github"].Active {
		t.Errorf("review %+v, merge %v %v", review, by["merge-local"].Active, by["merge-github"].Active)
	}
	out.Reset()
	if code := Main(context.Background(), []string{"dev", "capabilities"}, &out, &errs, nil); code != entry.ExitOK ||
		!strings.Contains(out.String(), "intake: active\n") || !strings.Contains(out.String(), "launch: active\n") ||
		!strings.Contains(out.String(), "review: inactive until P22a; rows not kept: none; pending: P31's qualification on record") {
		t.Errorf("the text form: %d %q", code, out.String())
	}
	if code := Main(context.Background(), []string{"dev", "capabilities", "extra"}, &out, &errs, nil); code != entry.ExitUsage {
		t.Errorf("an extra argument: exit %d", code)
	}
}
