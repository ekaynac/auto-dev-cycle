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
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// issuing are the actions whose fresh applied response carries a token,
// with how a token that was not delivered is recovered.
var issuing = map[string]string{
	"agent.enrol": "revoke the actor (adc admin revoke-actor) and enrol it again under a new actor id",
}

// writeToken is TokenOut.Write; a test fails it.
var writeToken = (*client.TokenOut).Write

// deliver writes an applied response's token to out, then drops it from
// the response, which is printed as usual; done reports an exit instead.
func deliver(name string, v *types.CommandApplied, out *client.TokenOut, recovery string, stderr io.Writer) (code int, done bool) {
	fail := func(format string, a ...any) (int, bool) {
		fmt.Fprintf(stderr, "%s: could_not_run: %s\n", name, fmt.Sprintf(format, a...))
		return entry.ExitCouldNotRun, true
	}
	switch {
	case v.IssuedCredential == nil && out == nil:
		return 0, false
	case out == nil:
		return fail("the command applied and issued a credential this command does not take; it was discarded and nothing was written: revoke the issued token")
	case v.IssuedCredential == nil:
		var r struct {
			Token string `json:"token"`
		}
		_ = json.Unmarshal(v.Result, &r)
		return fail("the command applied earlier (token %q) and a repeat delivers no token; %s", r.Token, recovery)
	}
	tok, err := identity.ParseSecret(*v.IssuedCredential)
	v.IssuedCredential = nil
	if err == nil {
		err = writeToken(out, tok)
	}
	if err != nil {
		return fail("the command applied, but its token could not be written (%v) and is not delivered again; %s", err, recovery)
	}
	return 0, false
}
