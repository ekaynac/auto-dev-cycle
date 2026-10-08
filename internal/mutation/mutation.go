// SPDX-License-Identifier: Apache-2.0

// Package mutation holds the named mutations that the M0 contract fixtures
// must turn red (docs/design/m0/11 "M0 exercises real validators"; 00 "M0
// exit check"). A mutation site reads `if mutation.Is(name) { … }`. In every
// ordinary build Is is a constant false, so the sites compile away; only a
// build with the adcmutation tag (make fixtures-contract) can switch one on,
// through ADC_MUTATION.
package mutation

// The named mutations, one per contract fixture that targets it.
const (
	// IdempotencyIgnoresInput makes the writer replay a stored result for a
	// reused key whatever the new input is.
	IdempotencyIgnoresInput = "eventstore.idempotency-ignores-input"
	// ExpectedStateUnchecked makes the writer skip the expected-state check.
	ExpectedStateUnchecked = "eventstore.expected-state-unchecked"
	// LockNotExclusive makes the state directory lock ignore a failed
	// exclusive flock, so a second controller could start.
	LockNotExclusive = "eventstore.lock-not-exclusive"
	// FenceIgnoresClosedPublication lets output from an attempt whose
	// publication is closed (stopped, lost, superseded) land.
	FenceIgnoresClosedPublication = "lease.fence-ignores-closed-publication"
	// IdentityIgnoresLineage leaves lineage and restrictions out of a ref's
	// identity digest, so identical bytes with another history are one ref.
	IdentityIgnoresLineage = "knowledge.identity-ignores-lineage"
	// CoverageTrustsDeclarations lets adapter declarations count as
	// evidence, so a declared-only turn no longer lowers coverage.
	CoverageTrustsDeclarations = "knowledge.coverage-trusts-declarations"
	// OuterStatusTrusted lets the producer's own ok (an empty
	// reported_status predicate) make the receipt clean, over the inner
	// predicates.
	OuterStatusTrusted = "receipt.outer-status-trusted"
	// AbsentPredicateUnchecked checks the evidence of the required
	// predicates present but not that each is present.
	AbsentPredicateUnchecked = "receipt.absent-predicate-unchecked"
	// UnknownFamilyIndependent compares "unknown" as one more family name,
	// so an unknown reviewer or proposer family no longer blocks.
	UnknownFamilyIndependent = "receipt.unknown-family-independent"
	// BindingDigestIgnored matches a review to a proposal by id alone, not
	// by the binding digest it names.
	BindingDigestIgnored = "receipt.binding-digest-ignored"
	// InheritedUsageNarrows reads dropping a usage override (inheriting the
	// installation's, which may be anything) as no widening.
	InheritedUsageNarrows = "authority.inherited-usage-narrows"
	// ProtectedCheckSupervisorsOnly refuses a widening protected change only
	// for supervisors, as if operators and the system were exempt.
	ProtectedCheckSupervisorsOnly = "authority.protected-check-supervisors-only"
	// StaleAttemptUsageDropped fences usage like output: the ledger counts
	// only the reports of the attempt that reported last.
	StaleAttemptUsageDropped = "ledger.stale-attempt-usage-dropped"
	// DeltasNotDeduplicated counts a repeated (call, sequence) delta again.
	DeltasNotDeduplicated = "ledger.deltas-not-deduplicated"
	// ChainHashUnchecked makes verification check each row's link to the
	// previous hash but not the row's own recomputed hash.
	ChainHashUnchecked = "eventstore.chain-hash-unchecked"
	// ConfirmIgnoresStatus lets a confirmation proceed on a proposal that
	// is no longer open (already confirmed or rejected).
	ConfirmIgnoresStatus = "authority.confirm-ignores-status"
	// ReservationsUncounted admits a reservation against its group's limit
	// alone, not counting what granted leases already hold.
	ReservationsUncounted = "capacity.reservations-uncounted"
	// SignatureUnchecked accepts a WebAuthn assertion without verifying its
	// signature over authenticatorData and the client data hash.
	SignatureUnchecked = "webauthn.signature-unchecked"
	// StreamCheckedAtOpenOnly authorizes an event stream when it opens and
	// never again, so a revoked grant keeps reading.
	StreamCheckedAtOpenOnly = "api.stream-checked-at-open-only"
	// IngestFollowsLinks resolves an adapter output's handle following a
	// symlink, so a link out of out/ is ingested as the file it names.
	IngestFollowsLinks = "adapterhost.ingest-follows-links"
	// IncompleteNoDiffEmpty reads a build that did not complete and changed
	// nothing as empty, the "no changes" a 402 before the first edit is not.
	IncompleteNoDiffEmpty = "graph.incomplete-no-diff-empty"
	// PartialPatchDropped drops the change of a build that did not complete,
	// so its partial work is neither the result's value nor sealed.
	PartialPatchDropped = "graph.partial-patch-dropped"
	// ClaimPerDemand claims a node again on every demand, so a repeat
	// demand runs it twice.
	ClaimPerDemand = "graph.claim-per-demand"
	// ResolveSkipsApplicability resolves an input to its producer's selected
	// result without asking whether its receipt applies.
	ResolveSkipsApplicability = "graph.resolve-skips-applicability"
	// DemandIgnored lets a receipt answer a requirement whatever demand it
	// answers, so identical conclusions for another demand apply.
	DemandIgnored = "receipt.demand-ignored"
	// RetryNeedsInvalidation refuses to clear a retry wait on its signal,
	// so a could_not_run result retries only after an invalidation.
	RetryNeedsInvalidation = "graph.retry-needs-invalidation"
	// RenewalInvalidates renews an expired result by invalidating its key,
	// a new generation, instead of reopening the same key.
	RenewalInvalidates = "graph.renewal-invalidates"
	// CommitSkipsAuthority commits a result whose answer input's approval
	// no longer answers its owed input.
	CommitSkipsAuthority = "graph.commit-skips-authority"
)

// Known lists every named mutation; a fixture golden may name only these.
var Known = []string{IdempotencyIgnoresInput, ExpectedStateUnchecked, LockNotExclusive, FenceIgnoresClosedPublication,
	IdentityIgnoresLineage, CoverageTrustsDeclarations,
	OuterStatusTrusted, AbsentPredicateUnchecked, UnknownFamilyIndependent, BindingDigestIgnored,
	InheritedUsageNarrows, ProtectedCheckSupervisorsOnly,
	StaleAttemptUsageDropped, DeltasNotDeduplicated, ChainHashUnchecked, ConfirmIgnoresStatus, ReservationsUncounted,
	SignatureUnchecked, StreamCheckedAtOpenOnly, IngestFollowsLinks, IncompleteNoDiffEmpty, PartialPatchDropped, ClaimPerDemand,
	ResolveSkipsApplicability, DemandIgnored, RetryNeedsInvalidation, RenewalInvalidates, CommitSkipsAuthority}
