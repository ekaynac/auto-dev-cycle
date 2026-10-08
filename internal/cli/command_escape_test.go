// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// booter is a fake that also answers the bootstrap.
type booter struct {
	fake
	boot string
	err  error
}

func (b *booter) Bootstrap(context.Context, []byte) ([]byte, error) { return []byte(b.boot), b.err }

// The command path (10a) writes the controller's text, and the caller's
// own, only through say: a hostile value in every field it shows is
// escaped, on stdout and stderr; --json escapes its C1 controls.
func TestHostileCommandFields(t *testing.T) {
	h, _ := json.Marshal(hostile)
	cmd := []string{"seat", "take", "--expect", "0", "--reason", "r"}
	for name, c := range map[string]struct {
		f    *fake
		args []string
		code int
	}{
		"refusal detail":            {&fake{resp: `{"disposition":"refused","outcome":"failed","reason":"seat_not_held","detail":` + string(h) + `}`}, cmd, entry.ExitRefused},
		"human scope":               {&fake{resp: human}, append(cmd, "--scope", "{\"a\":\"\u202e\u2066\u200b\u2028\"}"), entry.ExitProposed},
		"effective change":          {&fake{resp: strings.Replace(human, `"id":"grt_01m3250vkwffc979nzjw0g2sf9"`, `"id":`+string(h), 1)}, cmd, entry.ExitProposed},
		"review's effective change": {&fake{resp: strings.Replace(review, `"id":"grt_01m3250vkwffc979nzjw0g2sf9"`, `"id":`+string(h), 1)}, cmd, entry.ExitProposed},
		"transport":                 {&fake{err: errors.New(hostile)}, cmd, entry.ExitCouldNotRun},
		"401":                       {&fake{err: fmt.Errorf("%w %s", client.ErrUnauthenticated, hostile)}, cmd, entry.ExitRefused},
		"400":                       {&fake{err: fmt.Errorf("%w %s", client.ErrBadRequest, hostile)}, cmd, entry.ExitUsage},
		"unknown flag":              {&fake{}, append(cmd, "--"+hostile), entry.ExitUsage},
		"argument":                  {&fake{}, append(cmd, hostile), entry.ExitUsage},
		"expect":                    {&fake{}, []string{"seat", "take", "--reason", "r", "--expect", hostile}, entry.ExitUsage},
		"verb":                      {&fake{}, []string{"seat", hostile}, entry.ExitUsage},
	} {
		code, out, errb := adc(c.f, c.args...)
		if code != c.code || raw(out) || raw(errb) || !strings.Contains(strings.ToUpper(out+errb), "202E") {
			t.Errorf("%s: %d %q %q", name, code, out, errb)
		}
	}
	f := &fake{resp: "{\n\t\"disposition\":\"refused\",\r\n\"outcome\":\"failed\",\"reason\":\"seat_not_held\",\"detail\":\"a\u009bb\u2028c\"}"}
	if _, out, _ := adc(f, append(cmd, "--json")...); out != `{"disposition":"refused","outcome":"failed","reason":"seat_not_held","detail":"a\u009bb\u2028c"}`+"\n" {
		t.Errorf("--json: %q, want %q", out, `{"disposition":"refused","outcome":"failed","reason":"seat_not_held","detail":"a\u009bb\u2028c"}`+"\n")
	}
	// The bootstrap's refusal and its transport error.
	secret, _, err := identity.NewBootstrapSecret()
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string]*booter{
		"bootstrap refusal": {boot: `{"disposition":"refused","outcome":"failed","reason":"confirmation_refused","detail":` + string(h) + `}`},
		"bootstrap error":   {err: errors.New(hostile)},
	} {
		if _, out := bootstrapCLI(t, b, secret.Reveal()+"\n", "--secret-stdin"); raw(out) || !strings.Contains(out, `\u{202E}`) {
			t.Errorf("%s: %q", name, out)
		}
	}
}

// The caller's own --token-out name, a token write error and a failed
// write of the result are escaped too.
func TestHostileCommandLocalFields(t *testing.T) {
	enrol := []string{"admin", "enrol-agent", "--expect", "0", "--reason", "r", "--token-out"}
	name := strings.ReplaceAll(hostile, "/", "")
	code, out, errs := adc(&fake{resp: issuedResp}, append(enrol, filepath.Join(privateDir(t), name))...)
	if code != entry.ExitOK || raw(out) || raw(errs) || !strings.Contains(errs, `\u{202E}`) {
		t.Errorf("--token-out: %d %q %q", code, out, errs)
	}
	defer func(old func(*client.TokenOut, identity.Secret) error) { writeToken = old }(writeToken)
	writeToken = func(*client.TokenOut, identity.Secret) error { return errors.New(hostile) }
	if code, out, errs := adc(&fake{resp: issuedResp}, append(enrol, filepath.Join(privateDir(t), "t"))...); code != entry.ExitCouldNotRun || raw(out) || raw(errs) || !strings.Contains(errs, `\u{202E}`) {
		t.Errorf("token write: %d %q %q", code, out, errs)
	}
	var errb strings.Builder
	if Main(context.Background(), []string{"seat", "take", "--expect", "0", "--reason", "r"}, failingWriter{hostile}, &errb, &fake{resp: applied}) != entry.ExitOK ||
		raw(errb.String()) || !strings.Contains(errb.String(), `\u{202E}`) {
		t.Errorf("result write: %q", errb.String())
	}
}

// A replayed token id from the result and an invalid refusal's message
// are escaped (they reach the terminal through fail and couldNotRun).
func TestHostileCommandMessages(t *testing.T) {
	h, _ := json.Marshal(hostile)
	replayed := strings.Replace(applied, `"replayed":false`, `"replayed":true`, 1)
	replayed = strings.Replace(replayed, `"result":{}`, `"result":{"token":`+string(h)+`}`, 1)
	enrol := []string{"admin", "enrol-agent", "--expect", "0", "--reason", "r", "--token-out", filepath.Join(privateDir(t), "t")}
	if code, out, errs := adc(&fake{resp: replayed}, enrol...); code != entry.ExitCouldNotRun || raw(out) || raw(errs) || !strings.Contains(errs, `token \x1b`) && !strings.Contains(errs, `token \u{001B}`) {
		t.Errorf("replayed token: %d %q %q", code, out, errs)
	}
	// A reason its outcome does not allow, with a hostile detail.
	invalid := `{"disposition":"refused","outcome":"could_not_run","reason":"seat_not_held","detail":` + string(h) + `}`
	if code, out, errs := adc(&fake{resp: invalid}, "seat", "take", "--expect", "0", "--reason", "r"); code != entry.ExitCouldNotRun || raw(out) || raw(errs) ||
		!strings.Contains(errs, "the controller's refusal is invalid") {
		t.Errorf("invalid refusal: %d %q %q", code, out, errs)
	}
}
