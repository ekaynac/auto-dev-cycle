// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// Credential is a destination's credential as the gateway holds it. It is
// never logged: printing or marshalling it gives a placeholder, and only
// Reveal, at injection, returns the value. The value sits in a closure, so
// reflection cannot read it either (as identity.Secret).
type Credential struct{ value func() string }

const redacted = "[redacted credential]"

// NewCredential holds v as a credential.
func NewCredential(v string) Credential { return Credential{func() string { return v }} }

func (Credential) String() string   { return redacted }
func (Credential) GoString() string { return redacted }

// Format writes the placeholder for every verb and flag.
func (Credential) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// MarshalJSON refuses: a credential never reaches a document.
func (Credential) MarshalJSON() ([]byte, error) {
	return nil, errors.New("gateway: a credential is never marshalled")
}

// Reveal returns the value, for the injector only.
func (c Credential) Reveal() string {
	if c.value == nil {
		return ""
	}
	return c.value()
}

// Secrets holds the controller's credentials by reference (the registry's
// credential_ref), read afresh at every lookup. Values come from the
// controller's environment (EnvSecrets) or a secret store, never from the
// sandbox, a request or the source tree.
type Secrets interface {
	// Credential is the credential named ref; ok is false when none is
	// held or its value is empty. An error never quotes a value.
	Credential(ref types.Slug) (c Credential, ok bool, err error)
}

// EnvSecrets reads ADC_CREDENTIAL_<REF> from the controller's environment,
// the reference upper-cased with every '-' as '_'.
type EnvSecrets struct{}

// EnvName is the variable that holds ref. A reference holding '_' has
// none: it would share one with the reference spelling it '-', and no
// spelling of '_' in a portable variable name keeps the map one-to-one and
// readable.
func EnvName(ref types.Slug) (string, bool) {
	if strings.Contains(ref, "_") {
		return "", false
	}
	return "ADC_CREDENTIAL_" + strings.ToUpper(strings.ReplaceAll(ref, "-", "_")), true
}

var errNoEnvName = errors.New("gateway: a credential reference holding '_' has no environment variable")

func (EnvSecrets) Credential(ref types.Slug) (Credential, bool, error) {
	name, ok := EnvName(ref)
	if !ok {
		return Credential{}, false, errNoEnvName
	}
	v, ok := os.LookupEnv(name)
	return NewCredential(v), ok && v != "", nil
}
