// SPDX-License-Identifier: Apache-2.0

package types_test

import (
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

var (
	dgA = "sha256:" + strings.Repeat("a", 64)
	dgB = "sha256:" + strings.Repeat("b", 64)
)

// Documents in schema property order, so a faithful decode and re-encode
// reproduces them byte for byte.
var receipts = map[string]string{
	"tree": `{"id":"rcp_01m3250v57fhm9v1z9xd1wk8gw","kind":"gate","instrument":{"name":"go-test","version":"1.0.0","digest":"` + dgA + `"},` +
		`"subject":{"type":"tree","project":"prj_01m3250vkwffc979nzjw0g2sf9","tree":"git:` + strings.Repeat("c", 40) + `","head":null,` +
		`"environment":{"identity":"` + dgA + `","description":"linux"},"contract_revision":"3","demand":{"key":"` + dgB + `","run":"run_01m3250v03fpq92bcrrpmkpsj9"}},` +
		`"provenance":{"command":"cmd_01m3250vz2ezn81cgvbkcn17jd","attempt":null,"calls":[],"model_families":[],"contributors":[]},` +
		`"produced_at":"2026-10-03T10:00:00Z","valid_until":null,` +
		`"predicates":[{"name":"tests","outcome":"found","reason":"gate_failures","detail":"2 failed","evidence":[{"blob":"` + dgB + `","media_type":"application/junit+xml","artifact":"junit"}],"payload":{"failed":2}}],` +
		`"outcome":"found","reason":"gate_failures","detail":"2 failed","verdict":null,"conclusions_digest":"` + dgB + `","supersedes":null}`,
	"review of a proposal": `{"id":"rcp_01m3250v57fhm9v1z9xd1wk8gw","kind":"review","instrument":{"name":"astra","version":"6","digest":"` + dgA + `"},` +
		`"subject":{"type":"proposal","proposal":"prp_01m3250vm7f6ktw39dszqnyddr","binding_digest":"` + dgA + `","demand":null},` +
		`"provenance":{"command":"cmd_01m3250vz2ezn81cgvbkcn17jd","attempt":"att_01m3250vnfeyrv67x6w0ddqr6a","calls":["cal_01m3250v9xft08ssr4ygq6g2np"],"model_families":["gpt"],"contributors":["act_01m3250v0aer9ss2097svmefj8"]},` +
		`"produced_at":"2026-10-03T10:00:00Z","valid_until":"2026-10-04T10:00:00Z","predicates":[],` +
		`"outcome":"empty","reason":"none","detail":"","verdict":"go","conclusions_digest":"` + dgB + `","supersedes":"rcp_01m3250v57ft7aehkqwq0yrq3n"}`,
}

const instrument = `{"name":"go-test","version":"1.0.0","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","subject_type":"tree",` +
	`"required_predicates":[{"name":"tests","evidence_required":["application/junit+xml"]}],"optional_predicates":[],` +
	`"preconditions":["toolchain_present"],"payload_schema":null,"artifact_classes":{"junit":"diagnostic"}}`

func TestReceiptsRoundTripThroughTheContract(t *testing.T) {
	for name, doc := range receipts {
		t.Run(name, func(t *testing.T) {
			r, err := contract.Decode[types.Receipt]("records/receipt", []byte(doc))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			out, err := contract.Encode("records/receipt", r)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(out) != doc {
				t.Fatalf("round trip changed the document:\n in: %s\nout: %s", doc, out)
			}
		})
	}
}

func TestReceiptSubjectVariantIsTyped(t *testing.T) {
	r, err := contract.Decode[types.Receipt]("records/receipt", []byte(receipts["tree"]))
	if err != nil {
		t.Fatal(err)
	}
	tree, ok := r.Subject.Variant.(*types.TreeSubject)
	if !ok || tree.Demand == nil || tree.Demand.Run != "run_01m3250v03fpq92bcrrpmkpsj9" || tree.Head != nil {
		t.Fatalf("subject decoded as %#v", r.Subject.Variant)
	}
}

func TestInstrumentContractRoundTrips(t *testing.T) {
	c, err := contract.Decode[types.InstrumentContract]("records/instrument-contract", []byte(instrument))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := contract.Encode("records/instrument-contract", c)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(out) != instrument {
		t.Fatalf("round trip changed the document:\n in: %s\nout: %s", instrument, out)
	}
}
