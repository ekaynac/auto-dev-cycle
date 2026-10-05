// SPDX-License-Identifier: Apache-2.0

package exitmap

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

func TestReason(t *testing.T) {
	for name, c := range map[string]struct {
		exit Exit
		want vocab.Reason
	}{
		"nothing observed":                {Exit{}, vocab.ReasonExecutorIncomplete},
		"only other":                      {Exit{Hints: []string{"other"}}, vocab.ReasonExecutorIncomplete},
		"an unknown hint":                 {Exit{Hints: []string{"teapot"}}, vocab.ReasonExecutorIncomplete},
		"a 4xx":                           {Exit{Hints: []string{"http_4xx"}}, vocab.ReasonProviderRequestRefused},
		"a 5xx":                           {Exit{Hints: []string{"http_5xx"}}, vocab.ReasonProviderUnavailable},
		"an idle cut":                     {Exit{Hints: []string{"idle_cut"}}, vocab.ReasonStreamIdleCut},
		"degenerate output":               {Exit{Hints: []string{"degenerate"}}, vocab.ReasonDegenerateOutput},
		"a rate limit":                    {Exit{Hints: []string{"rate_limited"}}, vocab.ReasonQuotaExhausted},
		"credit exhausted":                {Exit{Hints: []string{"credit_exhausted"}}, vocab.ReasonBrokerCreditExhausted},
		"a stop":                          {Exit{Stopped: true}, vocab.ReasonAttemptStopped},
		"a protocol break":                {Exit{Protocol: true}, vocab.ReasonAdapterProtocolError},
		"credit before quota":             {Exit{Hints: []string{"rate_limited", "credit_exhausted"}}, vocab.ReasonBrokerCreditExhausted},
		"credit kept behind a stop":       {Exit{Stopped: true, Hints: []string{"credit_exhausted"}}, vocab.ReasonBrokerCreditExhausted},
		"quota kept behind a break":       {Exit{Protocol: true, Hints: []string{"rate_limited"}}, vocab.ReasonQuotaExhausted},
		"a stop before a break":           {Exit{Stopped: true, Protocol: true}, vocab.ReasonAttemptStopped},
		"a stop before a provider error":  {Exit{Stopped: true, Hints: []string{"http_5xx"}}, vocab.ReasonAttemptStopped},
		"a break before a provider error": {Exit{Protocol: true, Hints: []string{"idle_cut"}}, vocab.ReasonAdapterProtocolError},
		"4xx before 5xx, in any order":    {Exit{Hints: []string{"http_5xx", "http_4xx"}}, vocab.ReasonProviderRequestRefused},
		"5xx before an idle cut":          {Exit{Hints: []string{"idle_cut", "http_5xx"}}, vocab.ReasonProviderUnavailable},
		"an idle cut before degenerate":   {Exit{Hints: []string{"degenerate", "idle_cut"}}, vocab.ReasonStreamIdleCut},
		"other does not mask a cause":     {Exit{Hints: []string{"other", "degenerate"}}, vocab.ReasonDegenerateOutput},
	} {
		if got := Reason(c.exit); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

// Every reason the table can give is registered for could_not_run, the
// outcome of an executor that did not complete.
func TestReasonsAreCouldNotRun(t *testing.T) {
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	reasons := []vocab.Reason{vocab.ReasonAttemptStopped, vocab.ReasonAdapterProtocolError, vocab.ReasonExecutorIncomplete}
	for _, c := range append(capacity, provider...) {
		reasons = append(reasons, c.reason)
	}
	for _, r := range reasons {
		if err := reg.Check(vocab.Result{Outcome: vocab.CouldNotRun, Reason: r, Detail: "x"}); err != nil {
			t.Errorf("%s: %v", r, err)
		}
	}
}

// The table covers every class_hint the adapter event schema allows: each
// is mapped, or is "other", which names no cause.
func TestEveryHintIsMapped(t *testing.T) {
	b, err := os.ReadFile("../../../schemas/adapter/event.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	enum := doc.Defs["ErrorEvent"].Properties["class_hint"].Enum
	if len(enum) == 0 {
		t.Fatal("no class_hint enum in schemas/adapter/event.json")
	}
	var mapped []string
	for _, c := range append(capacity, provider...) {
		mapped = append(mapped, c.hint)
	}
	for _, h := range enum {
		if h != "other" && !slices.Contains(mapped, h) {
			t.Errorf("class_hint %s is not mapped", h)
		}
	}
	for _, h := range mapped {
		if !slices.Contains(enum, h) {
			t.Errorf("mapped hint %s is not a class_hint", h)
		}
	}
}
