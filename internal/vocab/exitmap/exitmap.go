// SPDX-License-Identifier: Apache-2.0

// Package exitmap is the one table that turns an executor's exit into a
// reason (docs/design/m0/02 "Outcomes come from observed facts", 13 "Facts,
// not claims"). The outcome is never the adapter's: an executor that did not
// complete is could_not_run, whatever it reported, and its exit only fills
// the reason.
package exitmap

import "github.com/ekaynac/auto-dev-cycle/internal/vocab"

// Exit is what the controller knows of an executor that did not complete.
type Exit struct {
	// StopRequested: the controller recorded the attempt's stop_requested
	// (05), which closes its publication. It does not mean execution or
	// billing ended (cancel is best effort, 09); the reason never waits for
	// that.
	StopRequested bool
	// Protocol: the adapter broke the contract, or its process ended
	// without a result.
	Protocol bool
	// Statuses are the HTTP statuses the controller itself observed for the
	// call's upstream requests (its gateway, or its own classification of an
	// error's raw excerpt): facts, where Hints are the adapter's claims.
	Statuses []int
	// Hints are the class_hint values of the call's error events
	// (schemas/adapter/event.json), in any order.
	Hints []string
}

// capacity are the causes whose reasons set holds (02): they come first, so
// a credit or quota hold is never lost behind a stop or a later error.
var capacity = []struct {
	hint   string
	reason vocab.Reason
}{
	{"credit_exhausted", vocab.ReasonBrokerCreditExhausted},
	{"rate_limited", vocab.ReasonQuotaExhausted},
}

// provider are the other causes, most specific first.
var provider = []struct {
	hint   string
	reason vocab.Reason
}{
	{"http_4xx", vocab.ReasonProviderRequestRefused},
	{"http_5xx", vocab.ReasonProviderUnavailable},
	{"idle_cut", vocab.ReasonStreamIdleCut},
	{"degenerate", vocab.ReasonDegenerateOutput},
}

// cause is the class an observed HTTP status shows, as a hint would name it:
// 402 is credit exhaustion and 429 a rate limit whatever the adapter hinted.
func cause(status int) string {
	switch {
	case status == 402:
		return "credit_exhausted"
	case status == 429:
		return "rate_limited"
	case status >= 400 && status < 500:
		return "http_4xx"
	case status >= 500 && status < 600:
		return "http_5xx"
	}
	return ""
}

// Reason is the reason for an executor that did not complete. The causes
// are the observed statuses and the hints together. Precedence, independent
// of the order they arrived in: capacity causes (credit, then quota), then
// the controller's stop request, then a protocol break, then the provider
// causes; with nothing that names a cause, executor_incomplete. "other",
// unknown hints and statuses outside 4xx and 5xx name no cause.
func Reason(e Exit) vocab.Reason {
	has := map[string]bool{}
	for _, h := range e.Hints {
		has[h] = true
	}
	for _, s := range e.Statuses {
		has[cause(s)] = true
	}
	for _, c := range capacity {
		if has[c.hint] {
			return c.reason
		}
	}
	switch {
	case e.StopRequested:
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
