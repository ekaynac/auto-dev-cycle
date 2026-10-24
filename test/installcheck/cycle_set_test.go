// SPDX-License-Identifier: Apache-2.0

package installcheck

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// P24b, D1 (Opus #435 L4): make installed-cycle-stub's set (the Makefile's
// STUB_CASES) names only cases that exist and can pass today: never one
// whose prerequisite has not landed. Each awaiting case is listed here with
// what it waits for; the PR that lands the prerequisite removes it here and
// adds it to STUB_CASES.
func TestStubCasesAwaitNothing(t *testing.T) {
	awaiting := map[string]string{
		"launch-missing-image": "Build307's fix: an image not present at its digest is an immediate wrong_owner hold (#226 entry 193)",
		"fault-provider-402":   "Build307's fix: a broker_credit_exhausted hold, no attempts spent to budget_rail (#226 entry 193)",
		"fault-provider-429":   "Build307's fix: a quota_reset hold until the reset (#226 entry 193)",
		"hostile-model-switch": "the driver sends no request from inside the attempt's container (gateway_requests)",
	}
	b, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^STUB_CASES := ((?:[^\n]*\\\n)*[^\n]*)$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("the Makefile defines no STUB_CASES")
	}
	cases := strings.Fields(strings.ReplaceAll(string(m[1]), "\\\n", " "))
	if len(cases) == 0 || !slices.Contains(cases, "candidate") {
		t.Fatalf("STUB_CASES: %v", cases)
	}
	for _, c := range cases {
		if why, ok := awaiting[c]; ok {
			t.Errorf("STUB_CASES names %s, which awaits %s", c, why)
		}
		if _, err := os.Stat("../../fixtures/m1/cases/" + c + "/case.json"); err != nil {
			t.Errorf("STUB_CASES names %s, which is no case: %v", c, err)
		}
	}
	for c := range awaiting {
		if _, err := os.Stat("../../fixtures/m1/cases/" + c + "/case.json"); err != nil {
			t.Errorf("the awaiting case %s is no case: %v", c, err)
		}
	}
}
