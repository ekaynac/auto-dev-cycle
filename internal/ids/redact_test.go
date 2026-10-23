// SPDX-License-Identifier: Apache-2.0

package ids

import "testing"

// Mythos chunk 5a L3: Redact finds ids by the schema's TypeID pattern,
// fresh ones included, and nothing else.
func TestRedact(t *testing.T) {
	a, err := New("evt")
	if err != nil {
		t.Fatal(err)
	}
	b, err := New("cmd")
	if err != nil {
		t.Fatal(err)
	}
	in := "event " + a + " of command " + b + ": node nodx_123 and abc_short"
	if got, want := Redact(in, "<id>"), "event <id> of command <id>: node nodx_123 and abc_short"; got != want {
		t.Errorf("Redact = %q, want %q", got, want)
	}
}
