// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

// adc task cancel's reason (Opus #298 L5; Astra #314 M1, L2): --reason
// fills a scope that names none; the scope is read strictly first, so a
// duplicate key, literal or escaped, is refused (exit 64, nothing sent),
// never resolved by re-encoding; a scope reason that differs from --reason
// is refused, the same one passes.
func TestTaskCancelReason(t *testing.T) {
	const t1, t2 = "tsk_01m3250v9xft08ssr4ygq6g2np", "tsk_01m3250v9xft08ssr4ygq6g2nq"
	cancel := func(reason, scope string) (int, *fake) {
		f := &fake{resp: applied}
		code, _, _ := run2(f, "task", "cancel", "--expect", "4", "--reason", reason, "--json", "--scope", scope)
		return code, f
	}
	code, f := cancel("stop", `{"task":"`+t1+`"}`)
	var sent struct{ Task, Reason string }
	if code != entry.ExitOK || json.Unmarshal(f.req.Scope, &sent) != nil || sent.Task != t1 || sent.Reason != "stop" {
		t.Fatalf("the fallback: %d %s", code, f.req.Scope)
	}
	for name, scope := range map[string]string{
		"a duplicate key":             `{"task":"` + t1 + `","task":"` + t2 + `"}`,
		"an escaped duplicate":        `{"task":"` + t1 + `","\u0074ask":"` + t2 + `"}`,
		"a duplicate reason":          `{"task":"` + t1 + `","reason":"stop","reason":"stop"}`,
		"a scope reason that differs": `{"task":"` + t1 + `","reason":"other"}`,
		"a scope that is no JSON":     `{"task":`,
	} {
		if code, f := cancel("stop", scope); code != entry.ExitUsage || f.calls != 0 {
			t.Errorf("%s: %d, %d sent", name, code, f.calls)
		}
	}
	if code, f := cancel("stop", `{"task":"`+t1+`","reason":"stop"}`); code != entry.ExitOK || f.calls != 1 {
		t.Errorf("the same reason twice: %d", code)
	}
}
