// SPDX-License-Identifier: Apache-2.0

package cli

// `adc admin bootstrap` (04 "Bootstrap", 14 R4, R5): the one-time setup
// secret, read from the state directory's file or from stdin (never argv,
// which other users can list), goes over the private socket; the token
// that comes back is written to ~/.config/adc/token and never printed.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// Bootstrapper is a Client that also sends the one-time bootstrap.
type Bootstrapper interface {
	Bootstrap(ctx context.Context, body []byte) ([]byte, error)
}

// stdin is what --secret-stdin reads; tests replace it.
var stdin io.Reader = os.Stdin

const maxSecretBytes = 256

// recoverHint is where a spent bootstrap's lost token comes back from.
const recoverHint = "the bootstrap is spent; recover the operator's token offline with adcd recover operator-token"

func adminBootstrap(ctx context.Context, args []string, stdout, stderr io.Writer, c Client) int {
	const name = "adc admin bootstrap"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateDir := fs.String("state-dir", "", "read the secret from bootstrap.secret in this state directory")
	fromStdin := fs.Bool("secret-stdin", false, "read the secret from stdin instead")
	opName := fs.String("name", "operator", "the first operator's display name")
	key := fs.String("idempotency-key", "", "repeat the request safely with the same key (default: a new key)")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		return entry.ExitOK
	} else if err != nil {
		return entry.ExitUsage
	}
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, name+": "+format+"\n", a...)
		return code
	}
	// Before the secret is read: the bootstrap is the private socket's [R4].
	if tr, ok := c.(client.Transport); ok && tr.Remote != nil {
		return fail(entry.ExitUsage, "%v", client.ErrRemoteBootstrap)
	}
	if fs.NArg() > 0 || (*stateDir == "") == !*fromStdin {
		return fail(entry.ExitUsage, "give exactly one of --state-dir DIR and --secret-stdin (never the secret as an argument)")
	}
	secret, err := readSecret(*stateDir)
	if err != nil {
		return fail(entry.ExitUsage, "%v", err)
	}
	b, ok := c.(Bootstrapper)
	if !ok {
		return fail(entry.ExitCouldNotRun, "could_not_run: %v", ErrNoTransport)
	}
	if *key == "" {
		if *key, err = newKey(); err != nil {
			return fail(entry.ExitCouldNotRun, "could_not_run: %v", err)
		}
	}
	// Validated as it is encoded; an error never quotes the secret.
	body, err := contract.Encode("api/bootstrap-request", types.BootstrapRequest{IdempotencyKey: *key, Secret: secret.Reveal(), Name: *opName})
	if err != nil {
		return fail(entry.ExitUsage, "the bootstrap request is invalid (check --name and --idempotency-key)")
	}
	raw, err := b.Bootstrap(ctx, body)
	if err != nil {
		return fail(entry.ExitCouldNotRun, "could_not_run: %v; if it was applied, a repeat (--idempotency-key %s) delivers no token: %s", err, *key, recoverHint)
	}
	resp, err := contract.Decode[types.BootstrapResponse]("api/bootstrap-response", raw)
	if err != nil {
		return fail(entry.ExitCouldNotRun, "could_not_run: the controller's response is invalid")
	}
	switch v := resp.Variant.(type) {
	case *types.BootstrapRefused:
		code := entry.ExitRefused
		if v.Outcome == types.BootstrapRefusedOutcomeCouldNotRun {
			code = entry.ExitCouldNotRun
		}
		return fail(code, "refused: %s/%s: %s", v.Outcome, v.Reason, v.Detail)
	case *types.BootstrapApplied:
		if v.IssuedCredential == nil {
			return fail(entry.ExitCouldNotRun, "could_not_run: a repeat of an applied bootstrap carries no token; %s", recoverHint)
		}
		tok, err := identity.ParseSecret(*v.IssuedCredential)
		files, ferr := client.DefaultFiles()
		if err = errors.Join(err, ferr); err == nil {
			err = files.Write(tok)
		}
		if err != nil {
			return fail(entry.ExitCouldNotRun, "could_not_run: the token could not be written (%v); %s", err, recoverHint)
		}
		fmt.Fprintf(stdout, "bootstrapped: operator %s; token %s written to ~/.config/adc/token\n", v.Operator, v.Token)
		return entry.ExitOK
	}
	return fail(entry.ExitCouldNotRun, "could_not_run: the controller's response is invalid")
}

// readSecret reads the secret from dir's bootstrap.secret (no link
// followed, never blocking on a FIFO or a terminal: a regular file, checked
// on the descriptor) or, with dir empty, from stdin.
func readSecret(dir string) (identity.Secret, error) {
	r := stdin
	if dir != "" {
		fd, err := unix.Open(filepath.Join(dir, identity.BootstrapFile), unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return identity.Secret{}, fmt.Errorf("the state directory's bootstrap.secret cannot be opened: %w", err)
		}
		f := os.NewFile(uintptr(fd), identity.BootstrapFile)
		defer f.Close()
		if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
			return identity.Secret{}, errors.New("bootstrap.secret is not a regular file")
		}
		r = f
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxSecretBytes))
	if err != nil {
		return identity.Secret{}, errors.New("the secret cannot be read")
	}
	return identity.ParseBootstrapSecret(strings.TrimSpace(string(raw)))
}
