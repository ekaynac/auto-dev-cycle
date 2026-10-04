// SPDX-License-Identifier: Apache-2.0

package receipt

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	dgA     = types.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	dgB     = types.Digest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	junit   = types.Digest("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	schemaD = types.Digest("sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	prjID   = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sf9")
	prpID   = types.ProposalID("prp_01m3250vm7f6ktw39dszqnyddr")
)

// fakeBlobs is a blob store of digest → bytes.
type fakeBlobs map[string][]byte

func (f fakeBlobs) Has(d string) (bool, error) { _, ok := f[d]; return ok, nil }
func (f fakeBlobs) Open(d string) (io.ReadCloser, error) {
	b, ok := f[d]
	if !ok {
		return nil, errors.New("no such blob")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func blobs() fakeBlobs {
	return fakeBlobs{string(junit): []byte("<testsuite/>"), string(schemaD): []byte(`{"type":"object","properties":{"passed":{"type":"integer"}},"required":["passed"],"additionalProperties":false}`)}
}

func contract() types.InstrumentContract {
	s := schemaD
	return types.InstrumentContract{Name: "go-test", Version: "1.0.0", Digest: dgA, SubjectType: types.InstrumentContractSubjectTypeTree,
		RequiredPredicates: []types.InstrumentContractRequiredPredicatesItem{{Name: "tests", EvidenceRequired: []string{"application/junit+xml"}}},
		OptionalPredicates: []string{"reported_status"}, Preconditions: []string{}, PayloadSchema: &s,
		ArtifactClasses: map[string]types.InstrumentContractArtifactClassesValue{"junit": "diagnostic"}}
}

func tree(t string) types.Subject {
	return types.Subject{Variant: &types.TreeSubject{Type: "tree", Project: prjID, Tree: types.GitID("git:" + strings.Repeat(t, 40)),
		Environment: types.Environment{Identity: dgB, Description: "linux/amd64"}, ContractRevision: "3"}}
}

func pred(name string, o vocab.Outcome, reason vocab.Reason, detail string, ev ...types.PredicateEvidenceItem) types.Predicate {
	if ev == nil {
		ev = []types.PredicateEvidenceItem{}
	}
	return types.Predicate{Name: name, Outcome: types.Outcome(o), Reason: reason, Detail: detail, Evidence: ev}
}

var junitEvidence = types.PredicateEvidenceItem{Blob: junit, MediaType: "application/junit+xml", Artifact: "junit"}

func receipt(ps ...types.Predicate) types.Receipt {
	return types.Receipt{Kind: types.ReceiptKindGate, Instrument: types.InstrumentRef{Name: "go-test", Version: "1.0.0", Digest: dgA},
		Subject: tree("c"), Provenance: types.ReceiptProvenance{ModelFamilies: []string{}}, ProducedAt: "2026-10-04T10:00:00Z", Predicates: ps}
}

func clean() types.Predicate {
	return pred("tests", vocab.Empty, vocab.ReasonCompletedClean, "", junitEvidence)
}

func derive(t *testing.T, r types.Receipt) Derived {
	t.Helper()
	d, err := Derive(contract(), r, blobs())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCompleteCleanReceipt(t *testing.T) {
	d := derive(t, receipt(clean()))
	if d.Outcome != vocab.Empty || d.Reason != vocab.ReasonCompletedClean || d.Detail != "" || d.Verdict != nil {
		t.Fatalf("%+v", d)
	}
}

// 06 fixtures: an omitted required predicate, missing evidence media, an
// evidence blob that is not published. Each is could_not_run, never empty.
func TestIncompleteNeverDerivesEmpty(t *testing.T) {
	noMedia := pred("tests", vocab.Empty, vocab.ReasonCompletedClean, "")
	unpublished := pred("tests", vocab.Empty, vocab.ReasonCompletedClean, "", types.PredicateEvidenceItem{Blob: dgB, MediaType: "application/junit+xml", Artifact: "junit"})
	for name, r := range map[string]types.Receipt{
		"omitted required predicate": receipt(),
		"only an optional predicate": receipt(pred("reported_status", vocab.Empty, vocab.ReasonCompletedClean, "")),
		"missing evidence media":     receipt(noMedia),
		"unpublished evidence":       receipt(unpublished),
	} {
		d := derive(t, r)
		if d.Outcome != vocab.CouldNotRun || d.Reason != vocab.ReasonReceiptIncomplete || !strings.HasPrefix(d.Detail, "missing ") {
			t.Errorf("%s: %s/%s %q", name, d.Outcome, d.Reason.String(), d.Detail)
		}
	}
}

// 06 fixture: a nested outer-ok/inner-stale lie: the producer's own status
// says ok, an inner predicate failed; the receipt is failed.
func TestOuterOKInnerFailedIsFailed(t *testing.T) {
	r := receipt(pred("reported_status", vocab.Empty, vocab.ReasonCompletedClean, ""),
		pred("tests", vocab.Failed, vocab.ReasonGateTestFailed, "TestParse failed", junitEvidence))
	d := derive(t, r)
	if d.Outcome != vocab.Failed || d.Reason != vocab.ReasonGateTestFailed || d.Detail != "tests: TestParse failed" {
		t.Fatalf("%+v", d)
	}
}

func TestAggregationOrder(t *testing.T) {
	found := pred("tests", vocab.Found, vocab.ReasonGateTestFailed, "one finding", junitEvidence)
	cnr := pred("reported_status", vocab.CouldNotRun, vocab.ReasonRunnerFault, "runner died")
	if d := derive(t, receipt(found, cnr)); d.Outcome != vocab.CouldNotRun {
		t.Errorf("found + could_not_run gave %s", d.Outcome)
	}
	if d := derive(t, receipt(found)); d.Outcome != vocab.Found {
		t.Errorf("found gave %s", d.Outcome)
	}
}

func TestReviewVerdictIsDerived(t *testing.T) {
	r := receipt(clean())
	r.Kind = types.ReceiptKindReview
	if d := derive(t, r); d.Verdict == nil || *d.Verdict != types.ReceiptVerdictValueGo {
		t.Fatalf("a clean review: %+v", d)
	}
	r.Predicates = []types.Predicate{pred("tests", vocab.Found, vocab.ReasonGateTestFailed, "a blocking finding", junitEvidence)}
	if d := derive(t, r); d.Verdict == nil || *d.Verdict != types.ReceiptVerdictValueNoGo {
		t.Fatalf("a review with a finding: %+v", d)
	}
	r.Predicates = nil
	if d := derive(t, r); d.Verdict == nil || *d.Verdict != types.ReceiptVerdictValueNoGo {
		t.Fatalf("an incomplete review: %+v", d)
	}
}

// Derived fields are never accepted from the producer.
func TestCheckRefusesRecordedFields(t *testing.T) {
	r := receipt(pred("tests", vocab.Failed, vocab.ReasonGateTestFailed, "x", junitEvidence))
	d := derive(t, r)
	r.Outcome, r.Reason, r.Detail, r.ConclusionsDigest = types.Outcome(d.Outcome), d.Reason, d.Detail, d.ConclusionsDigest
	if err := Check(contract(), r, blobs()); err != nil {
		t.Fatalf("the derived fields: %v", err)
	}
	for name, mutate := range map[string]func(*types.Receipt){
		"outcome":     func(r *types.Receipt) { r.Outcome, r.Reason, r.Detail = "empty", vocab.ReasonCompletedClean, "" },
		"detail":      func(r *types.Receipt) { r.Detail = "all good" },
		"verdict":     func(r *types.Receipt) { v := types.ReceiptVerdictValueGo; r.Verdict = &v },
		"conclusions": func(r *types.Receipt) { r.ConclusionsDigest = dgB },
	} {
		c := r
		mutate(&c)
		if err := Check(contract(), c, blobs()); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Rule 7 and the contract: a receipt that does not fit its instrument is
// invalid, never derived.
func TestInvalidReceipts(t *testing.T) {
	payload := func(p types.Predicate, js string) types.Predicate { p.Payload = []byte(js); return p }
	noSchema := contract()
	noSchema.PayloadSchema = nil
	cases := map[string]struct {
		c types.InstrumentContract
		r types.Receipt
	}{
		"another instrument digest": {contract(), func() types.Receipt { r := receipt(clean()); r.Instrument.Digest = dgB; return r }()},
		"undeclared predicate":      {contract(), receipt(clean(), pred("coverage", vocab.Empty, vocab.ReasonCompletedClean, ""))},
		"predicate twice":           {contract(), receipt(clean(), clean())},
		"undeclared artifact": {contract(), receipt(pred("tests", vocab.Empty, vocab.ReasonCompletedClean, "",
			types.PredicateEvidenceItem{Blob: junit, MediaType: "application/junit+xml", Artifact: "stderr"}))},
		"payload without a schema":   {noSchema, receipt(payload(clean(), `{"passed": 3}`))},
		"payload failing the schema": {contract(), receipt(payload(clean(), `{"passed": "three"}`))},
	}
	for name, c := range cases {
		if _, err := Derive(c.c, c.r, blobs()); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Derive(contract(), receipt(payload(clean(), `{"passed": 3}`)), blobs()); err != nil {
		t.Fatalf("a valid payload: %v", err)
	}
}

// Rule 2a: the conclusions digest covers the demand and each predicate's
// outcome and reason, not their order or detail.
func TestConclusionsDigest(t *testing.T) {
	a := derive(t, receipt(clean(), pred("reported_status", vocab.Empty, vocab.ReasonCompletedClean, "ok")))
	b := derive(t, receipt(pred("reported_status", vocab.Empty, vocab.ReasonCompletedClean, "different words"), clean()))
	if a.ConclusionsDigest != b.ConclusionsDigest {
		t.Fatal("order or detail changed the conclusions")
	}
	c := derive(t, receipt(pred("tests", vocab.Found, vocab.ReasonGateTestFailed, "x", junitEvidence)))
	if c.ConclusionsDigest == a.ConclusionsDigest {
		t.Fatal("a changed outcome kept the conclusions")
	}
	withDemand := receipt(clean())
	sub := tree("c")
	sub.Variant.(*types.TreeSubject).Demand = &types.DemandValue{Key: dgB, Run: "run_01m3250v03fpq92bcrrpmkpsj9"}
	withDemand.Subject = sub
	if derive(t, withDemand).ConclusionsDigest == derive(t, receipt(clean())).ConclusionsDigest {
		t.Fatal("the demand is not in the conclusions")
	}
}

func requirement() Requirement {
	return Requirement{Instrument: types.InstrumentRef{Name: "go-test", Version: "1.0.0", Digest: dgA}, Subject: tree("c")}
}

var now = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func refusal(t *testing.T, err error) vocab.Reason {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return r.Reason
}

// 06 fixtures: an instrument upgrade, a tree mismatch; and the other
// applicability rules.
func TestApplicability(t *testing.T) {
	r := receipt(clean())
	if err := Applies(r, requirement(), now); err != nil {
		t.Fatalf("the same instrument and subject: %v", err)
	}
	another := requirement()
	another.Instrument.Name = "py-test"
	if got := refusal(t, Applies(r, another, now)); got != vocab.ReasonEvidenceInapplicable {
		t.Errorf("another instrument: %s", got.String())
	}
	upgraded := requirement()
	upgraded.Instrument.Digest = dgB
	if got := refusal(t, Applies(r, upgraded, now)); got != vocab.ReasonInstrumentChanged {
		t.Errorf("an instrument upgrade: %s", got.String())
	}
	other := requirement()
	other.Subject = tree("e")
	if got := refusal(t, Applies(r, other, now)); got != vocab.ReasonEvidenceInapplicable {
		t.Errorf("a tree mismatch: %s", got.String())
	}
	env := requirement()
	env.Subject.Variant.(*types.TreeSubject).Environment.Identity = dgA
	if got := refusal(t, Applies(r, env, now)); got != vocab.ReasonEvidenceInapplicable {
		t.Errorf("another environment: %s", got.String())
	}
	rev := requirement()
	rev.Subject.Variant.(*types.TreeSubject).ContractRevision = "4"
	if got := refusal(t, Applies(r, rev, now)); got != vocab.ReasonEvidenceInapplicable {
		t.Errorf("another contract revision: %s", got.String())
	}
	demand := requirement()
	k := dgB
	demand.DemandKey = &k
	if got := refusal(t, Applies(r, demand, now)); got != vocab.ReasonEvidenceInapplicable {
		t.Errorf("a receipt for no demand against a demand: %s", got.String())
	}
	expiring := r
	until := types.Time("2026-10-04T11:00:00Z")
	expiring.ValidUntil = &until
	if got := refusal(t, Applies(expiring, requirement(), now)); got != vocab.ReasonEvidenceInapplicable {
		t.Errorf("an expired receipt: %s", got.String())
	}
}

// 06 fixture: a GO reused for an unrelated proposal never applies.
func TestGOForAnotherProposal(t *testing.T) {
	r := receipt(clean())
	r.Kind = types.ReceiptKindReview
	r.Subject = types.Subject{Variant: &types.ProposalSubject{Type: "proposal", Proposal: prpID, BindingDigest: dgA}}
	req := requirement()
	req.Subject = types.Subject{Variant: &types.ProposalSubject{Type: "proposal", Proposal: prpID, BindingDigest: dgA}}
	if err := Applies(r, req, now); err != nil {
		t.Fatalf("the bound proposal: %v", err)
	}
	req.Subject = types.Subject{Variant: &types.ProposalSubject{Type: "proposal", Proposal: prpID, BindingDigest: dgB}}
	if got := refusal(t, Applies(r, req, now)); got != vocab.ReasonEvidenceInapplicable {
		t.Errorf("another binding digest: %s", got.String())
	}
}

// 06 fixture: an unknown reviewer family blocks; independence is mechanical.
func TestIndependence(t *testing.T) {
	r := receipt(clean())
	r.Provenance.ModelFamilies = []string{"claude"}
	if err := Independent(r, []string{"gpt"}); err != nil {
		t.Fatalf("disjoint families: %v", err)
	}
	for name, c := range map[string]struct {
		fams, excluded []string
	}{
		"shared family":    {[]string{"claude"}, []string{"claude"}},
		"unknown reviewer": {[]string{"unknown"}, []string{"gpt"}},
		"no family":        {[]string{}, []string{"gpt"}},
		"unknown proposer": {[]string{"claude"}, []string{"unknown"}},
	} {
		r.Provenance.ModelFamilies = c.fams
		if got := refusal(t, Independent(r, c.excluded)); got != vocab.ReasonReviewerNotIndependent {
			t.Errorf("%s: %s", name, got.String())
		}
	}
}

// Rule 5: artifacts are copied before teardown.
func TestCanTeardown(t *testing.T) {
	if err := CanTeardown(receipt(clean()), blobs()); err != nil {
		t.Fatal(err)
	}
	missing := receipt(pred("tests", vocab.Empty, vocab.ReasonCompletedClean, "", types.PredicateEvidenceItem{Blob: dgB, MediaType: "application/junit+xml", Artifact: "junit"}))
	if err := CanTeardown(missing, blobs()); err == nil {
		t.Fatal("teardown with an artifact not in the blob store")
	}
}
