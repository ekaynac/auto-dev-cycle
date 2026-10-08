// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/daemon"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
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
	home := testguard.Home(t) // never the user's real ~/.config/adc
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
	home := testguard.Home(t)
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

// A malformed bootstrap response that carries a credential never reaches
// the terminal: the validation error is fixed text.
func TestAdminBootstrapMalformedResponse(t *testing.T) {
	testguard.Home(t)
	dir, err := os.MkdirTemp("", "adc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "adcd.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	const secretPart = "zzFAKEzzSECRETzzRECOGNIZABLEzzQQQQQQQQQQQQA"
	bearer := "tok_01m3250vkwffc979nzjw0g2sa1." + secretPart
	body := `{"disposition":"applied","command":"cmd_01m3250vkwffc979nzjw0g2sa1","replayed":false,"operator":"act_01m3250vkwffc979nzjw0g2sa1","token":"tok_01m3250vkwffc979nzjw0g2sa1","issued_credential":"` + bearer + `!"}`
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, body) })}
	go srv.Serve(l)
	defer srv.Close()
	env := map[string]string{SocketEnv: socket}
	c := ClientFromEnv(func(k string) string { return env[k] })
	s, _, err := identity.NewBootstrapSecret()
	if err != nil {
		t.Fatal(err)
	}
	code, out := bootstrapCLI(t, c, s.Reveal(), "--secret-stdin")
	if code != entry.ExitCouldNotRun || strings.Contains(out, secretPart) || strings.Contains(out, s.Reveal()) {
		t.Fatalf("%d %s", code, out)
	}
}

// The secret file must be a regular file: a FIFO is refused at once (never
// waited on), and so are a symlink and a directory.
func TestAdminBootstrapSecretFile(t *testing.T) {
	testguard.Home(t)
	// The link's target holds a valid secret: following it would get as
	// far as the transport.
	s, _, err := identity.NewBootstrapSecret()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "real")
	if err := os.WriteFile(target, []byte(s.Reveal()), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		plant func(p string) error
		says  string
	}{
		"a FIFO":      {func(p string) error { return syscall.Mkfifo(p, 0o600) }, "not a regular file"},
		"a symlink":   {func(p string) error { return os.Symlink(target, p) }, "cannot be opened"},
		"a directory": {func(p string) error { return os.Mkdir(p, 0o700) }, "not a regular file"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := c.plant(filepath.Join(dir, identity.BootstrapFile)); err != nil {
				t.Fatal(err)
			}
			type result struct {
				code int
				out  string
			}
			done := make(chan result, 1)
			go func() {
				code, out := bootstrapCLI(t, NoTransport{}, "", "--state-dir", dir)
				done <- result{code, out}
			}()
			select {
			case r := <-done:
				if r.code != entry.ExitUsage || !strings.Contains(r.out, c.says) {
					t.Fatalf("exit %d: %s", r.code, r.out)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the reader blocked")
			}
		})
	}
}
