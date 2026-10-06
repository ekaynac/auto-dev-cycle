// SPDX-License-Identifier: Apache-2.0

package client

import (
	"errors"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

// EncodeRequest is the body of POST /v1/commands/{action}, validated
// against schemas/api/command-request.json as it is written.
func EncodeRequest(r types.CommandRequest) ([]byte, error) {
	return contract.Encode("api/command-request", r)
}

// checkRequest refuses a body that is not a valid CommandRequest; the bytes
// checked are the bytes sent.
func checkRequest(body []byte) error {
	return contract.Validate("api/command-request", body)
}

// DecodeResponse validates a response against
// schemas/api/command-response.json and decodes it. It fails closed: a
// disposition the schema does not name is refused, and so is a decoded
// value of no known variant.
func DecodeResponse(raw []byte) (types.CommandResponse, error) {
	r, err := contract.Decode[types.CommandResponse]("api/command-response", raw)
	if err != nil {
		return types.CommandResponse{}, err
	}
	switch r.Variant.(type) {
	case *types.CommandApplied, *types.CommandProposed, *types.CommandRefused:
		return r, nil
	}
	return types.CommandResponse{}, errors.New("the controller's response has no known disposition")
}
