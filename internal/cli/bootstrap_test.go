// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/daemon"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// adcd runs an in-process daemon on a fresh state directory (short, so its
// socket path fits) and returns the directory and the client to it.
func adcd(t *testing.T) (string, Client) {
	t.Helper()
	dir, err := os.MkdirTemp("", "adcd")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done, ready := make(chan error, 1), make(chan struct{})
	go func() {
		done <- daemon.Run(ctx, daemon.Config{StateDir: dir}, io.Discard, func(*daemon.Daemon) { close(ready) })
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("adcd: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		os.RemoveAll(dir)
	})
	env := map[string]string{SocketEnv: filepath.Join(dir, "adcd.sock")}
	return dir, ClientFromEnv(func(k string) string { return env[k] })
}

func bootstrapCLI(t *testing.T, c Client, in string, args ...string) (int, string) {
	t.Helper()
	stdin = strings.NewReader(in)
	t.Cleanup(func() { stdin = os.Stdin })
	var out, errb strings.Builder
	code := Main(context.Background(), append([]string{"admin", "bootstrap"}, args...), &out, &errb, c)
	return code, out.String() + errb.String()
}

// The secret from the state directory bootstraps once and the token lands
// in ~/.config/adc/token, never on the terminal; a repeat of the same key
// delivers no token; the spent secret, from stdin, is refused; the secret
// is never an argument.
func TestAdminBootstrap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // never the user's real ~/.config/adc
	dir, c := adcd(t)
	raw, err := os.ReadFile(filepath.Join(dir, identity.BootstrapFile))
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimSpace(string(raw))
	for _, args := range [][]string{{}, {"--state-dir", dir, "--secret-stdin"}, {"--secret-stdin", secret}} {
		if code, _ := bootstrapCLI(t, c, secret, args...); code != entry.ExitUsage {
			t.Errorf("%d args: exit %d", len(args), code)
		}
	}
	code, out := bootstrapCLI(t, c, "", "--state-dir", dir, "--idempotency-key", "k1")
	if code != entry.ExitOK || !strings.Contains(out, "bootstrapped: operator act_") {
		t.Fatalf("bootstrap: %d %s", code, out)
	}
	files := client.Files{Home: home, UID: os.Getuid()}
	tok, err := files.Load()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, secret) || strings.Contains(out, tok.Reveal()) {
		t.Fatal("a secret reached the terminal")
	}
	code, out = bootstrapCLI(t, c, secret+"\n", "--secret-stdin", "--idempotency-key", "k1")
	if code != entry.ExitCouldNotRun || !strings.Contains(out, "carries no token") || !strings.Contains(out, "recover operator-token") {
		t.Fatalf("repeat: %d %s", code, out)
	}
	if code, out = bootstrapCLI(t, c, secret, "--secret-stdin"); code != entry.ExitRefused || !strings.Contains(out, "confirmation_refused") {
		t.Fatalf("spent: %d %s", code, out)
	}
	if again, err := files.Load(); err != nil || again.Reveal() != tok.Reveal() {
		t.Fatal("the token file changed")
	}
}

// A token-file write that fails after the commit says how to recover and
// prints no token.
func TestAdminBootstrapTokenWriteFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".config"), nil, 0o600); err != nil { // not a directory
		t.Fatal(err)
	}
	dir, c := adcd(t)
	code, out := bootstrapCLI(t, c, "", "--state-dir", dir)
	if code != entry.ExitCouldNotRun || !strings.Contains(out, "could not be written") || !strings.Contains(out, "recover operator-token") ||
		regexp.MustCompile(`tok_[0-9a-z]{26}\.`).MatchString(out) {
		t.Fatalf("a failed write: %d %s", code, out)
	}
}
