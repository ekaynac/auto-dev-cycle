// SPDX-License-Identifier: Apache-2.0

package schemas_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

// A union encodes its variant on contract.Encode's own encoder, so invalid
// UTF-8 inside it is refused, never repaired to U+FFFD and then accepted
// (Astra #44 round 2).
func TestUnionsEncodeStrictly(t *testing.T) {
	b, err := json.Marshal(receipt("tree"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := contract.Decode[types.Receipt]("records/receipt", b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contract.Encode("records/receipt", r); err != nil {
		t.Fatalf("a valid receipt: %v", err)
	}
	r.Subject.Variant.(*types.TreeSubject).ContractRevision = string([]byte{0xff})
	if _, err := contract.Encode("records/receipt", r); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 inside a discriminated union: %v", err)
	}

	g, err := json.Marshal(authorityDocs()["records/grant"])
	if err != nil {
		t.Fatal(err)
	}
	grant, err := contract.Decode[types.Grant]("records/grant", g)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contract.Encode("records/grant", grant); err != nil {
		t.Fatalf("a valid grant: %v", err)
	}
	grant.Projects.List[0] = types.ProjectID("prj_" + string([]byte{0xff}))
	// The repaired id would also fail adc-typeid, so check the refusal comes
	// from marshalling.
	if _, err := contract.Encode("records/grant", grant); err == nil || !strings.Contains(err.Error(), "marshal:") || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 inside a literal-or-list union: %v", err)
	}
}
