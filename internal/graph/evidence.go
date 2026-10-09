// SPDX-License-Identifier: Apache-2.0

package graph

// Evidence attempts, caps and preconditions (12 "Attempts keyed by demand",
// "Preconditions"; plan 15 E2).
//
// Decisions:
//   - An attempt is a recorded result of an evidence node. It is counted per
//     (subject, instrument digest, demand key) and in total per (subject,
//     instrument digest), across demands. The subject is the content of the
//     result its subject_input resolves to (its one Ref's content digest),
//     so the same tree re-produced shares one budget. The counts live in
//     graph_attempts, keyed apart from the computation key, so an
//     invalidation, a renewal or a retry never resets them (03 rule 1).
//   - Each count binds the cap it was first counted under. A node whose
//     caps differ from a bound cap is refused at claim and by the count
//     fold; raising a bound cap is an M1 policy change.
//   - The fold refuses a counted result once either cap is spent: the node
//     records could_not_run/evidence_cap_exhausted instead, which the fold
//     admits only when a cap is spent, and Exhaust holds it on wrong_owner.
//   - A could_not_run result spends no attempt when its reason says nothing
//     ran (notRun); every other result spends one. Every could_not_run
//     reason of the registry is in notRun or in ran.
//   - The preconditions are the instrument contract's (06): only Probe holds
//     a precondition failure, on the probe of one of the contract's
//     preconditions ("probe:<precondition>"), and Retry refuses it. The
//     hold fold binds only the prefix of a builder's signal (probe:, caps:):
//     which precondition or result follows it is checked by the builder.
//   - An evidence node is claimed only through ClaimEvidence, under its
//     contract: the instrument by name, version and digest, and a subject
//     input whose result kind is the contract's subject_type (in M0 only a
//     tree subject has result kinds: tree, package). The evaluator checks
//     the contract again before it renews an evidence node's key.

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/evidence"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// AttemptProjection counts evidence attempts: subject/digest/demand and
// subject/digest → attempts spent and the cap they are bound to.
const AttemptProjection = "graph_attempts"

// Contracts is the instrument contract registered under ref (06).
type Contracts func(ref types.InstrumentRef) (types.InstrumentContract, error)

// subjectKinds are the result kinds that are a subject of each type.
var subjectKinds = map[types.InstrumentContractSubjectType][]types.ResultKind{
	types.InstrumentContractSubjectTypeTree: {types.ResultKindTree, types.ResultKindPackage},
}

// notRun and ran split the could_not_run reasons (reasons.yaml) by whether
// the evidence run started: a notRun result spends no attempt.
var (
	notRun = []string{"precondition_failed", "evidence_cap_exhausted", "capacity_reserved_full", "quota_unknown", "action_deferred",
		"project_unconfigured", "controller_already_running", "store_unreadable", "ambiguous_state", "reviewer_not_independent", "instrument_changed",
		"check_unloadable", "view_stale", "evidence_inapplicable"}
	ran = []string{"quota_exhausted", "broker_credit_exhausted", "runner_fault", "receipt_incomplete",
		"attempt_stopped", "adapter_protocol_error", "provider_request_refused", "provider_unavailable", "stream_idle_cut", "degenerate_output",
		"executor_incomplete", "isolation_unproven"}
)

// count is one counter and the cap it is bound to.
type count struct {
	Spent int64 `json:"spent"`
	Cap   int64 `json:"cap"`
}

// budget is an evidence node's counters, checked against its caps.
type budget struct {
	demandKey, subjectKey string
	demand, subject       count
}

func (b budget) spent() bool {
	return b.demand.Spent >= b.demand.Cap || b.subject.Spent >= b.subject.Cap && !mutation.Is(mutation.SubjectCapUnchecked)
}

// budgetFor reads the counters of node n's params p on subject, the result
// its subject input resolves to, and binds them to p's caps.
func budgetFor(tx *eventstore.Tx, p types.EvidenceNodeParams, subject types.ResultID) (budget, error) {
	var b budget
	if p.Limits.MaxAttemptsPerDemand > p.Limits.MaxAttemptsPerSubject {
		return b, errors.New("graph: an evidence cap per demand exceeds its cap per subject")
	}
	r, _, err := load[types.ResultRecorded](tx, ResultProjection, string(subject))
	if err != nil || len(r.Result.Value) != 1 {
		return b, errors.Join(err, fmt.Errorf("graph: subject %s holds no one Ref", subject))
	}
	demand := "plan"
	if p.Demand.Key != nil && !mutation.Is(mutation.AttemptsIgnoreDemand) {
		demand = string(*p.Demand.Key)
	}
	b.subjectKey = string(r.Result.Value[0].ContentDigest) + "/" + string(p.Instrument.Digest)
	b.demandKey = b.subjectKey + "/" + demand
	var derr, serr error
	b.demand, _, derr = load[count](tx, AttemptProjection, b.demandKey)
	b.subject, _, serr = load[count](tx, AttemptProjection, b.subjectKey)
	for _, c := range []struct {
		n   *count
		cap int64
	}{{&b.demand, p.Limits.MaxAttemptsPerDemand}, {&b.subject, p.Limits.MaxAttemptsPerSubject}} {
		if c.n.Cap != 0 && c.n.Cap != c.cap {
			return b, fmt.Errorf("graph: the evidence cap %d is not the bound cap %d", c.cap, c.n.Cap)
		}
		c.n.Cap = c.cap
	}
	return b, errors.Join(derr, serr)
}

// budgetOf is rec's budget on the subject its claim resolved.
func budgetOf(tx *eventstore.Tx, rec nodeRecord) (budget, error) {
	p, err := rec.Recorded.Node.ParamsAsEvidenceNodeParams()
	c, ok, cerr := load[claim](tx, ClaimProjection, string(rec.Recorded.Node.ID))
	if err = errors.Join(err, cerr); err != nil || !ok {
		return budget{}, errors.Join(err, errors.New("graph: an unclaimed evidence node has no budget"))
	}
	i := slices.IndexFunc(c.Inputs, func(in types.ClaimedInput) bool { return in.Name == p.SubjectInput })
	if i < 0 || c.Inputs[i].Result == nil {
		return budget{}, fmt.Errorf("graph: the claim resolved no subject %s", p.SubjectInput)
	}
	return budgetFor(tx, p, *c.Inputs[i].Result)
}

// Exhausted reports whether node, a claimed evidence node, has spent a cap:
// its next result is could_not_run/evidence_cap_exhausted.
func Exhausted(tx *eventstore.Tx, node types.NodeID) (bool, error) {
	rec, err := loadNode(tx, node)
	if err != nil {
		return false, err
	}
	b, err := budgetOf(tx, rec)
	return b.spent(), err
}

// countAttempt is the fold's count of an evidence node's result r.
func countAttempt(tx *eventstore.Tx, rec nodeRecord, r types.Result) error {
	if rec.Recorded.Node.Kind != types.NodeKindEvidence {
		return nil
	}
	// What did not run is admitted before the budget is read, so a node that
	// lost the bind race still records its precondition failure.
	counted := r.Reason == vocab.ReasonPreconditionFailed && mutation.Is(mutation.PreconditionSpendsAttempt)
	if r.Outcome == types.OutcomeCouldNotRun && slices.Contains(notRun, r.Reason.String()) && r.Reason != vocab.ReasonEvidenceCapExhausted && !counted {
		return nil
	}
	b, err := budgetOf(tx, rec)
	switch {
	case err != nil:
		return err
	case r.Reason == vocab.ReasonEvidenceCapExhausted:
		if !b.spent() {
			return errors.New("an evidence node is not exhausted while a cap is unspent")
		}
		return nil
	case b.spent():
		return errors.New("the evidence node's attempts are spent: its result is evidence_cap_exhausted")
	}
	b.demand.Spent++
	b.subject.Spent++
	return errors.Join(put(tx, AttemptProjection, b.demandKey, b.demand), put(tx, AttemptProjection, b.subjectKey, b.subject))
}

// CheckContract checks an evidence node against its instrument contract c:
// the same instrument, its restore (evidence.CheckRestore), and a subject
// input of a kind of c's subject_type.
func CheckContract(n types.Node, c types.InstrumentContract) error {
	p, err := n.ParamsAsEvidenceNodeParams()
	if err != nil {
		return err
	}
	if p.Instrument.Name != c.Name || p.Instrument.Version != c.Version || p.Instrument.Digest != c.Digest {
		return fmt.Errorf("graph: the evidence node's instrument is not contract %s %s", c.Name, c.Version)
	}
	if err := evidence.CheckRestore(p, c); err != nil {
		return err
	}
	i := slices.IndexFunc(n.Inputs, func(in types.InputSpec) bool { return in.Name == p.SubjectInput })
	if i < 0 {
		return fmt.Errorf("graph: subject %s is no input of the node", p.SubjectInput)
	}
	src, ok := n.Inputs[i].Source.Variant.(*types.ResultSource)
	if !ok || !slices.Contains(subjectKinds[c.SubjectType], src.ResultKind) {
		return fmt.Errorf("graph: subject %s is no %s subject of contract %s", p.SubjectInput, c.SubjectType, c.Name)
	}
	return nil
}

// contractOf is the contract of rec's evidence node, checked against it.
func contractOf(rec nodeRecord, contracts Contracts) (types.InstrumentContract, error) {
	p, err := rec.Recorded.Node.ParamsAsEvidenceNodeParams()
	if err != nil || contracts == nil {
		return types.InstrumentContract{}, errors.Join(err, errors.New("graph: no instrument contracts"))
	}
	c, err := contracts(p.Instrument)
	if err != nil {
		return c, err
	}
	return c, CheckContract(rec.Recorded.Node, c)
}

// ClaimEvidence is Claim for an evidence node: under its contract, and
// with caps equal to those its counters are bound to.
func ClaimEvidence(tx *eventstore.Tx, node types.NodeID, resolved map[string]Resolved, generation int64, contracts Contracts) ([]eventstore.Event, error) {
	rec, err := loadNode(tx, node)
	if err != nil || rec.Recorded.Node.Kind != types.NodeKindEvidence {
		return nil, errors.Join(err, fmt.Errorf("graph: %s is no evidence node", node))
	}
	if _, err := contractOf(rec, contracts); err != nil {
		return nil, err
	}
	p, err := rec.Recorded.Node.ParamsAsEvidenceNodeParams()
	if err == nil {
		_, err = budgetFor(tx, p, resolved[string(p.SubjectInput)].Result)
	}
	if err != nil {
		return nil, err
	}
	return claimNode(tx, rec, resolved, generation)
}

// Probe holds node on precondition_failed for its could_not_run result,
// on the probe of precondition, which must be one of its contract's.
func Probe(tx *eventstore.Tx, node types.NodeID, result types.ResultID, precondition types.Name, hold types.NodeHold, envOwner *string, env Env) ([]eventstore.Event, error) {
	rec, err := loadNode(tx, node)
	if err != nil {
		return nil, err
	}
	c, err := contractOf(rec, env.Contracts)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(c.Preconditions, precondition) {
		return nil, fmt.Errorf("graph: %s is no precondition of contract %s", precondition, c.Name)
	}
	r, _, err := load[types.ResultRecorded](tx, ResultProjection, string(result))
	if err != nil || r.Result.Reason != vocab.ReasonPreconditionFailed {
		return nil, errors.Join(err, fmt.Errorf("graph: %s is no precondition failure", result))
	}
	hold.SignalInstance = "probe:" + string(precondition)
	return retry(tx, node, result, hold, envOwner)
}

// Exhaust holds node's could_not_run/evidence_cap_exhausted result on
// wrong_owner, owned by the project owner, whose act (in M0: retiring the
// node) is its signal.
func Exhaust(tx *eventstore.Tx, node types.NodeID, result types.ResultID, projectOwner string) ([]eventstore.Event, error) {
	r, _, err := load[types.ResultRecorded](tx, ResultProjection, string(result))
	id, ierr := ids.New("hld")
	if err = errors.Join(err, ierr); err != nil || r.Result.Reason != vocab.ReasonEvidenceCapExhausted {
		return nil, errors.Join(err, fmt.Errorf("graph: %s is no evidence cap exhaustion", result))
	}
	return retry(tx, node, result, types.NodeHold{ID: types.HoldID(id), SignalInstance: "caps:" + string(result),
		Owner: types.OwnerNamedOwner, OwnerRef: &projectOwner, Pages: []types.NodeHoldPagesItem{}}, nil)
}

// builtWaits are the retry waits only their builder raises, by the signal
// instance prefix the fold requires of them.
var builtWaits = map[string]string{"precondition_failed": "probe:", "evidence_cap_exhausted": "caps:"}

// builtWait: a retry wait for reason carries its builder's signal instance.
func builtWait(reason types.ReasonCode, signal string) error {
	if prefix, ok := builtWaits[reason.String()]; ok && !strings.HasPrefix(signal, prefix) {
		return fmt.Errorf("a %s wait is raised by its builder, on a %s signal", reason, prefix)
	}
	return nil
}
