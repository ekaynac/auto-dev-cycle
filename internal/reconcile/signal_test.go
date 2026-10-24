// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// 5c-1's Unblock decides on a typed signal: each runner, quota reset and
// credit probe signal reads back as the value that made it, the legacy
// "runner: image" among them (Astra #433 H1: a store from before #433 may
// hold it; it is read and offered, never raised now); a near miss is
// refused. Offered takes the hold: a result's retry wait
// (Reason set) is never the loop's, whatever its signal says, but a
// quota_reset on its reset time; a credit probe never (stated terminal);
// and a malformed signal of each kind is never offered; no_sandbox and
// review_inactive only on their own signals (#423).
func TestRunnerSignalsAndOffered(t *testing.T) {
	until := time.Date(2026, 10, 23, 1, 2, 3, 400, time.UTC)
	for _, r := range []RunnerHold{{What: "dirs"}, {What: "failed"}, {What: "sandbox"}, {What: "image"}, {What: "profile", Digest: "sha256:" + "abababababababababababababababababababababababababababababababab"},
		{What: "retry", N: 2, Until: until}} {
		if got, ok := ParseRunner(r.Signal()); !ok || got.Signal() != r.Signal() || got.What != r.What || !got.Until.Equal(r.Until) || got.N != r.N {
			t.Errorf("%+v: %q reads back %+v %v", r, r.Signal(), got, ok)
		}
	}
	for _, s := range []string{"runner:", "runner: ", "runner: dir", "runner: dirs ", "runner: profile sha256:ab", "runner: profile x",
		"runner: retry 0 until 2026-10-23T00:00:00Z", "runner: retry 1 until tomorrow", "runner: retry one until 2026-10-23T00:00:00Z",
		"runner:dirs", "Runner: dirs", "runner: sandbox now", "runner: image now", "runner: images"} {
		if r, ok := ParseRunner(s); ok {
			t.Errorf("%q reads as %+v", s, r)
		}
	}
	if q, ok := ParseQuotaReset(QuotaReset{Until: until}.Signal()); !ok || !q.Until.Equal(until) {
		t.Errorf("%q reads back %+v %v", QuotaReset{Until: until}.Signal(), q, ok)
	}
	for _, s := range []string{"quota: reset", "quota: reset ", "quota: reset tomorrow", "quota: reset 2026-10-23T01:02:03+02:00", "quota: reset 2026-10-23T00:00:00Z ",
		"quota:reset 2026-10-23T00:00:00Z", "Quota: reset 2026-10-23T00:00:00Z"} {
		if q, ok := ParseQuotaReset(s); ok {
			t.Errorf("%q reads as %+v", s, q)
		}
	}
	if c, ok := ParseCreditProbe(CreditProbe{Destination: "stub"}.Signal()); !ok || c.Destination != "stub" {
		t.Errorf("the credit probe reads back %+v %v", c, ok)
	}
	for _, s := range []string{"credit:", "credit: ", "credit: a b", "credit: Stub!", "credit:stub", "credit: stub\n"} {
		if c, ok := ParseCreditProbe(s); ok {
			t.Errorf("%q reads as %+v", s, c)
		}
	}
	quota := vocab.ReasonQuotaExhausted
	reason := vocab.ReasonPreconditionFailed
	for name, c := range map[string]struct {
		hold types.NodeHold
		want bool
	}{
		"a placement hold":                        {types.NodeHold{Hold: vocab.HoldSourceInvalidConfig, SignalInstance: "placement: executor exe_01m4tm99haemqt8es2qfbwj8pa"}, true},
		"a runner hold":                           {types.NodeHold{Hold: vocab.HoldSourceInvalidConfig, SignalInstance: "runner: dirs"}, true},
		"the legacy runner image hold":            {types.NodeHold{Hold: vocab.HoldSourceInvalidConfig, SignalInstance: "runner: image"}, true},
		"an image hold":                           {types.NodeHold{Hold: vocab.HoldWrongOwner, SignalInstance: "image docker.io/x@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, true},
		"a roster hold":                           {types.NodeHold{Hold: vocab.HoldReviewerUnavailable, SignalInstance: "registry:exe_01m4tm99haemqt8es2qfbwj8pa"}, true},
		"the day's rail":                          {types.NodeHold{Hold: vocab.HoldBudgetRail, SignalInstance: "rail:per_day_usd"}, true},
		"a retry wait on a placement-like signal": {types.NodeHold{Hold: vocab.HoldSourceInvalidConfig, Reason: &reason, SignalInstance: "placement: executor exe_01m4tm99haemqt8es2qfbwj8pa"}, false},
		"a retry wait on a runner-like signal":    {types.NodeHold{Hold: vocab.HoldSourceInvalidConfig, Reason: &reason, SignalInstance: "runner: dirs"}, false},
		"a retry wait on an image-like signal":    {types.NodeHold{Hold: vocab.HoldWrongOwner, Reason: &reason, SignalInstance: "image x"}, false},
		"a malformed placement signal":            {types.NodeHold{Hold: vocab.HoldSourceInvalidConfig, SignalInstance: "placement: executor e"}, false},
		"a malformed runner signal":               {types.NodeHold{Hold: vocab.HoldSourceInvalidConfig, SignalInstance: "runner: dirs and more"}, false},
		"a malformed image signal":                {types.NodeHold{Hold: vocab.HoldWrongOwner, SignalInstance: "image a b"}, false},
		"a malformed roster signal":               {types.NodeHold{Hold: vocab.HoldReviewerUnavailable, SignalInstance: "registry:someone"}, false},
		"a placement signal on another kind":      {types.NodeHold{Hold: vocab.HoldWrongOwner, SignalInstance: "placement: executor exe_01m4tm99haemqt8es2qfbwj8pa"}, false},
		"another day's rail":                      {types.NodeHold{Hold: vocab.HoldBudgetRail, SignalInstance: "rail:per_task_usd"}, false},
		// #226 entries 171, 173 (#423): no_sandbox on the runner's sandbox
		// signal alone, review_inactive on ReviewSignal alone.
		"no_sandbox":                                  {types.NodeHold{Hold: vocab.HoldNoSandbox, SignalInstance: "runner: sandbox"}, true},
		"no_sandbox on another runner signal":         {types.NodeHold{Hold: vocab.HoldNoSandbox, SignalInstance: "runner: dirs"}, false},
		"the sandbox signal on source_invalid_config": {types.NodeHold{Hold: vocab.HoldSourceInvalidConfig, SignalInstance: "runner: sandbox"}, false},
		"review_inactive":                             {types.NodeHold{Hold: vocab.HoldReviewInactive, SignalInstance: ReviewSignal}, true},
		"review_inactive on another signal":           {types.NodeHold{Hold: vocab.HoldReviewInactive, SignalInstance: "capability: launch"}, false},
		// #226 entry 193 (§2.6): the runner's quota wait on its reset time,
		// with its result's reason (graph.Retry) or not; never on another
		// signal, nor another hold on its signal; a credit probe never.
		"a quota reset":                        {types.NodeHold{Hold: vocab.HoldQuotaReset, Reason: &quota, SignalInstance: "quota: reset 2026-10-23T00:00:00Z"}, true},
		"a quota reset on a malformed time":    {types.NodeHold{Hold: vocab.HoldQuotaReset, Reason: &quota, SignalInstance: "quota: reset soon"}, false},
		"a quota reset on a runner signal":     {types.NodeHold{Hold: vocab.HoldQuotaReset, Reason: &quota, SignalInstance: "runner: retry 1 until 2026-10-23T00:00:00Z"}, false},
		"a retry wait on a quota reset signal": {types.NodeHold{Hold: vocab.HoldSourceInvalidConfig, Reason: &reason, SignalInstance: "quota: reset 2026-10-23T00:00:00Z"}, false},
		"a credit probe":                       {types.NodeHold{Hold: vocab.HoldBrokerCreditExhausted, Reason: &quota, SignalInstance: "credit: stub"}, false},
		"a credit probe with no reason":        {types.NodeHold{Hold: vocab.HoldBrokerCreditExhausted, SignalInstance: "credit: stub"}, false},
	} {
		if got := Offered(c.hold); got != c.want {
			t.Errorf("%s: offered %v, want %v", name, got, c.want)
		}
	}
}
