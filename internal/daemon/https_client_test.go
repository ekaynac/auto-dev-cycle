// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/cli"
	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// anchors are cert's trust anchors as the CLI takes them: a CA file (the
// certificate is self-signed) and its pin.
func (c testCert) anchors(t *testing.T) (ca, pin string) {
	t.Helper()
	ca = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, c.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(c.certPEM)
	d := sha256.Sum256(b.Bytes)
	return ca, client.PinPrefix + hex.EncodeToString(d[:])
}

// adcHTTPS runs the CLI against the HTTPS listener at addr with tok and
// the trust anchor env names.
func adcHTTPS(addr, tok string, anchor map[string]string, args ...string) (int, string, string) {
	env := map[string]string{cli.HTTPSAddrEnv: addr, client.TokenEnv: tok}
	for k, v := range anchor {
		env[k] = v
	}
	var out, errb bytes.Buffer
	code := cli.Main(context.Background(), args, &out, &errb, cli.ClientFromEnv(func(k string) string { return env[k] }))
	return code, out.String(), errb.String()
}

// The CLI over the daemon's HTTPS listener: a certificate verified by the
// configured CA or pin carries commands; a wrong CA or pin fails closed
// with fixed text (exit 2) and nothing is recorded; a wrong token is
// refused (exit 1).
func TestCLIOverHTTPS(t *testing.T) {
	c := runCommanded(t)
	cert := validCert(t)
	srv, _, _ := httpsCommandedWith(t, c, cert)
	addr := srv.tcp.String()
	ca, pin := cert.anchors(t)
	wrongCA, wrongPin := validCert(t).anchors(t)
	sup := c.sup.Reveal()

	code, out, errb := adcHTTPS(addr, sup, map[string]string{cli.HTTPSCAEnv: ca}, "seat", "take", "--expect", "0", "--reason", "r", "--json")
	a, ok := response(t, out).(*types.CommandApplied)
	if code != entry.ExitOK || !ok {
		t.Fatalf("take with the CA: %d %s %s", code, out, errb)
	}
	seat := strconv.FormatInt(a.LastSeq, 10)
	before := c.commandsOf(t)
	_, secret, _ := strings.Cut(sup, ".")
	for name, anchor := range map[string]map[string]string{"wrong ca": {cli.HTTPSCAEnv: wrongCA}, "wrong pin": {cli.HTTPSPinEnv: wrongPin}} {
		code, out, errb := adcHTTPS(addr, sup, anchor, "seat", "release", "--expect", seat, "--reason", "r")
		if code != entry.ExitCouldNotRun || out != "" || !strings.Contains(errb, client.ErrServerIdentity.Error()) || strings.Contains(errb, secret) {
			t.Errorf("%s: %d %q %q", name, code, out, errb)
		}
	}
	if n := c.commandsOf(t); n != before {
		t.Fatalf("a command over an unverified connection was recorded: %d, was %d", n, before)
	}
	if code, out, errb := adcHTTPS(addr, sup, map[string]string{cli.HTTPSPinEnv: pin}, "seat", "release", "--expect", seat, "--reason", "r"); code != entry.ExitOK || !strings.HasPrefix(out, "applied: cmd_") {
		t.Fatalf("release with the pin: %d %s %s", code, out, errb)
	}
	id, err := ids.New("tok")
	if err != nil {
		t.Fatal(err)
	}
	stranger := id + ".zzFAKEzzSECRETzzRECOGNIZABLEzzQQQQQQQQQQQQA"
	if code, out, errb := adcHTTPS(addr, stranger, map[string]string{cli.HTTPSPinEnv: pin}, "seat", "take", "--expect", "0", "--reason", "r"); code != entry.ExitRefused || out != "" || !strings.Contains(errb, "unauthenticated") || strings.Contains(errb, "zzFAKEzz") {
		t.Fatalf("a stranger's token: %d %q %q", code, out, errb)
	}
}

// A held-off address: the listener closes its connections before the TLS
// handshake, and the CLI says so in fixed text, exit 2.
func TestCLIHeldOffOverHTTPS(t *testing.T) {
	lim := tlsLimits()
	lim.freeFailures = 0
	s, _, _, cert := serveTLS(t, lim)
	addr := s.srv.tcp.String()
	_, pin := cert.anchors(t)
	id, err := ids.New("tok")
	if err != nil {
		t.Fatal(err)
	}
	wrong := id + ".zzFAKEzzSECRETzzRECOGNIZABLEzzQQQQQQQQQQQQA"
	anchor := map[string]string{cli.HTTPSPinEnv: pin}
	if code, _, errb := adcHTTPS(addr, wrong, anchor, "seat", "take", "--expect", "0", "--reason", "r"); code != entry.ExitRefused {
		t.Fatalf("a wrong token: %d %q", code, errb)
	}
	holdOff(s.srv.th, api.Peer{Addr: netip.MustParseAddr("127.0.0.1")}.Key())
	code, out, errb := adcHTTPS(addr, s.tok.Reveal(), anchor, "seat", "take", "--expect", "0", "--reason", "r")
	_, secret, _ := strings.Cut(s.tok.Reveal(), ".")
	if code != entry.ExitCouldNotRun || out != "" || !strings.Contains(errb, client.ErrHandshakeRefused.Error()) || strings.Contains(errb, secret) {
		t.Fatalf("held off: %d %q %q", code, out, errb)
	}
}
