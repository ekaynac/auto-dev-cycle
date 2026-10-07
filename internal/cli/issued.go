// SPDX-License-Identifier: Apache-2.0

package cli

// Issued credentials (08 "Issued credentials", 14 N3, M4): an action that
// issues a token issues it to another actor, or rotates one, so its token
// goes only to an explicit --token-out file and never to the caller's own
// token file, stdout or a log. A repeat of an applied command carries no
// token, so each action says how an undelivered one is recovered.

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// issuing are the actions whose fresh applied response carries a token,
// with how a token that was not delivered is recovered.
var issuing = map[string]string{
	"agent.enrol": "revoke the actor (adc admin revoke-actor) and enrol it again under a new actor id",
	"token.issue": "a repeat with the same --idempotency-key reports the token's id; revoke it (adc admin revoke-token) or rotate it (adc admin replace-token)",
	"token.replace": "the old token is revoked; a repeat with the same --idempotency-key reports the new token's id: rotate it (adc admin replace-token), " +
		"from another operator's token or, for your own last one, after adcd recover operator-token",
}

// writeToken is TokenOut.Write, and encodeResponse the response's
// encoding; tests fail them.
var (
	writeToken     = (*client.TokenOut).Write
	encodeResponse = func(r types.CommandResponse) ([]byte, error) { return contract.Encode("api/command-response", r) }
)

// deliver takes an applied response's credential out of it and writes it
// to out, returning the response re-encoded without it. The encoding comes
// first, so whatever is reported once the token is written is true; done
// reports an exit instead.
func deliver(name string, resp types.CommandResponse, v *types.CommandApplied, out *client.TokenOut, recovery string, stderr io.Writer) (clean []byte, code int, done bool) {
	fail := func(format string, a ...any) ([]byte, int, bool) {
		say(stderr, "%s: could_not_run: %s\n", name, fmt.Sprintf(format, a...))
		return nil, entry.ExitCouldNotRun, true
	}
	credential := v.IssuedCredential
	v.IssuedCredential = nil
	switch {
	case credential == nil && out == nil:
		return nil, 0, false
	case out == nil:
		return fail("the command applied and issued a credential this command does not take; it was discarded and nothing was written: revoke the issued token")
	case credential == nil && v.Replayed:
		var r struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(v.Result, &r); err != nil || r.Token == "" {
			r.Token = "not in the result"
		}
		return fail("the command applied earlier (token %q) and a repeat delivers no token; %s", r.Token, recovery)
	case credential == nil:
		return fail("the command applied but no credential was returned; %s", recovery)
	}
	clean, err := encodeResponse(resp)
	if err != nil {
		return fail("the command applied, but its response could not be re-encoded, so its token was not written and is not delivered again; %s", recovery)
	}
	tok, err := identity.ParseSecret(*credential)
	if err == nil {
		err = writeToken(out, tok)
	}
	if err != nil {
		return fail("the command applied, but its token could not be written (%v) and is not delivered again; %s", err, recovery)
	}
	return clean, 0, false
}
