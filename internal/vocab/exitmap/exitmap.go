// SPDX-License-Identifier: Apache-2.0

// Package exitmap is the one table that turns an executor's exit into a
// reason (docs/design/m0/02 "Outcomes come from observed facts", 13 "Facts,
// not claims"). The outcome is never the adapter's: an executor that did not
// complete is could_not_run, whatever it reported, and its exit only fills
// the reason.
package exitmap

import "github.com/ekaynac/auto-dev-cycle/internal/vocab"

// Exit is what the controller observed of an executor that did not
// complete.
type Exit struct {
	// Stopped: the controller stopped the attempt (05 attempt.stop_requested).
	Stopped bool
	// Protocol: the adapter broke the contract, or its process ended
	// without a result.
	Protocol bool
	// Hints are the class_hint values of the call's error events
	// (schemas/adapter/event.json), in any order.
	Hints []string
}

// capacity are the hints whose reasons set holds (02): they come first, so
// a credit or quota hold is never lost behind a stop or a later error.
var capacity = []struct {
	hint   string
	reason vocab.Reason
}{
	{"credit_exhausted", vocab.ReasonBrokerCreditExhausted},
	{"rate_limited", vocab.ReasonQuotaExhausted},
}

// provider are the other hints that name a cause, most specific first.
var provider = []struct {
	hint   string
	reason vocab.Reason
}{
	{"http_4xx", vocab.ReasonProviderRequestRefused},
	{"http_5xx", vocab.ReasonProviderUnavailable},
	{"idle_cut", vocab.ReasonStreamIdleCut},
	{"degenerate", vocab.ReasonDegenerateOutput},
}

// Reason is the reason for an executor that did not complete. Precedence,
// independent of the order the hints arrived in: capacity hints (credit,
// then quota), then the controller's own stop, then a protocol break, then
// the provider hints; with nothing that names a cause, executor_incomplete.
// "other" and unknown hints name no cause (a hint is the adapter's word).
func Reason(e Exit) vocab.Reason {
	has := map[string]bool{}
	for _, h := range e.Hints {
		has[h] = true
	}
	for _, c := range capacity {
		if has[c.hint] {
			return c.reason
		}
	}
	switch {
	case e.Stopped:
		return vocab.ReasonAttemptStopped
	case e.Protocol:
		return vocab.ReasonAdapterProtocolError
	}
	for _, p := range provider {
		if has[p.hint] {
			return p.reason
		}
	}
	return vocab.ReasonExecutorIncomplete
}
