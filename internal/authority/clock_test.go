// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// clockShiftEnv shifts the suite's clock (a Go duration). Folds judge
// expiry and validity only at an event's recorded time, which the store's
// clock stamps, so every time a test builds must come from that clock: a
// time written as a literal against it is a test that fails on the day the
// literal passes.
const clockShiftEnv = "ADC_TEST_CLOCK_SHIFT"

var clockShift = func() time.Duration {
	d, err := time.ParseDuration(os.Getenv(clockShiftEnv))
	if err != nil {
		return 0
	}
	return d
}()

// suiteNow is the suite's clock: the store's (eventstore.Config.Now) and
// the base of t0.
func suiteNow() time.Time { return time.Now().Add(clockShift) }

// TestSuiteSurvivesAShiftedClock runs this package's tests again with the
// clock five years ahead: a test whose times do not come from suiteNow
// fails there long before the real date reaches it.
func TestSuiteSurvivesAShiftedClock(t *testing.T) {
	if clockShift != 0 {
		t.Skip("already shifted")
	}
	cmd := exec.Command(os.Args[0], "-test.count=1", "-test.skip=^TestSuiteSurvivesAShiftedClock$")
	cmd.Env = append(os.Environ(), clockShiftEnv+"=43830h")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the suite fails with its clock five years ahead: %v\n%s", err, out)
	}
}
