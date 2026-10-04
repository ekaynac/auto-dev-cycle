// SPDX-License-Identifier: Apache-2.0

package schemas_test

import (
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

// decodeAny decodes an authority record into its generated type.
func decodeAny(schema string, b []byte) (any, error) {
	switch schema {
	case "records/actor":
		return contract.Decode[types.Actor](schema, b)
	case "records/grant":
		return contract.Decode[types.Grant](schema, b)
	case "records/approval":
		return contract.Decode[types.Approval](schema, b)
	case "records/proposal":
		return contract.Decode[types.Proposal](schema, b)
	case "records/challenge":
		return contract.Decode[types.Challenge](schema, b)
	case "records/owed-input":
		return contract.Decode[types.OwedInput](schema, b)
	}
	return nil, fmt.Errorf("no generated type for %s", schema)
}
