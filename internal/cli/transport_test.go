// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

func TestClientFromEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // never the user's real ~/.config/adc
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if _, ok := ClientFromEnv(getenv).(NoTransport); !ok {
		t.Fatal("nothing configured is not NoTransport")
	}
	env[SocketEnv] = "/run/x/adcd.sock"
	if tr, ok := ClientFromEnv(getenv).(client.Transport); !ok || tr.Socket != env[SocketEnv] || tr.DaemonUID != os.Getuid() {
		t.Fatalf("socket: %#v", tr)
	}
	env[DaemonUIDEnv] = "123"
	if tr, ok := ClientFromEnv(getenv).(client.Transport); !ok || tr.DaemonUID != 123 {
		t.Fatalf("daemon uid: %#v", tr)
	}
	env[DaemonUIDEnv] = "-1"
	if _, err := ClientFromEnv(getenv).Command(context.Background(), "task.create", nil); err == nil || !strings.Contains(err.Error(), DaemonUIDEnv) {
		t.Fatalf("a bad uid: %v", err)
	}
}

// A command goes over the socket ADC_SOCKET names with ADC_TOKEN; a peer of
// another uid is refused as could_not_run.
func TestCommandOverTheSocket(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, applied) })}
	go srv.Serve(l)
	defer srv.Close()
	id, err := ids.New("tok")
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{SocketEnv: socket, client.TokenEnv: id + ".zzFAKEzzSECRETzzRECOGNIZABLEzzQQQQQQQQQQQQA"}
	getenv := func(k string) string { return env[k] }
	run := func() (int, string) {
		var out, errb strings.Builder
		code := Main(context.Background(), []string{"task", "create", "--expect", "0", "--reason", "r"}, &out, &errb, ClientFromEnv(getenv))
		return code, out.String() + errb.String()
	}
	if code, out := run(); code != entry.ExitOK || !strings.HasPrefix(out, "applied:") {
		t.Fatalf("%d %s", code, out)
	}
	env[DaemonUIDEnv] = strconv.Itoa(os.Getuid() + 1)
	if code, out := run(); code != entry.ExitCouldNotRun || strings.Contains(out, "zzFAKEzz") {
		t.Fatalf("%d %s", code, out)
	}
}

// ADC_HTTPS_ADDR selects the HTTPS transport, with exactly one trust
// anchor; it and ADC_SOCKET together are refused; a bad configuration
// fails every command with its fixed text.
func TestClientFromEnvHTTPS(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	pin := client.PinPrefix + strings.Repeat("ab", 32)
	env := map[string]string{HTTPSAddrEnv: "127.0.0.1:8443", HTTPSPinEnv: pin}
	getenv := func(k string) string { return env[k] }
	if tr, ok := ClientFromEnv(getenv).(client.Transport); !ok || tr.Remote == nil || tr.Socket != "" {
		t.Fatalf("https: %#v", tr)
	}
	env[SocketEnv] = "/run/x/adcd.sock"
	if _, err := ClientFromEnv(getenv).Command(context.Background(), "task.create", nil); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("both transports: %v", err)
	}
	delete(env, SocketEnv)
	delete(env, HTTPSAddrEnv)
	for _, anchor := range []string{HTTPSPinEnv, HTTPSCAEnv} {
		env[SocketEnv], env[anchor] = "/run/x/adcd.sock", "x"
		if _, err := ClientFromEnv(getenv).Command(context.Background(), "task.create", nil); err == nil || !strings.Contains(err.Error(), "need "+HTTPSAddrEnv) {
			t.Fatalf("%s without %s: %v", anchor, HTTPSAddrEnv, err)
		}
		delete(env, anchor)
	}
	delete(env, SocketEnv)
	env[HTTPSAddrEnv] = "127.0.0.1:8443"
	if _, err := ClientFromEnv(getenv).Command(context.Background(), "task.create", nil); err == nil || !strings.Contains(err.Error(), "trust anchor") {
		t.Fatalf("no trust anchor: %v", err)
	}
}

// adc admin bootstrap with the HTTPS transport is a usage error, and the
// secret goes nowhere: the bootstrap stays unconsumed.
func TestBootstrapNotOverHTTPS(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, _ := adcd(t)
	secret, err := os.ReadFile(filepath.Join(dir, identity.BootstrapFile))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{HTTPSAddrEnv: "127.0.0.1:8443", HTTPSPinEnv: client.PinPrefix + strings.Repeat("ab", 32)}
	code, out := bootstrapCLI(t, ClientFromEnv(func(k string) string { return env[k] }), string(secret), "--secret-stdin")
	if code != entry.ExitUsage || !strings.Contains(out, client.ErrRemoteBootstrap.Error()) {
		t.Fatalf("%d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, identity.BootstrapFile)); err != nil {
		t.Fatalf("the bootstrap was consumed: %v", err)
	}
	// Refused before the secret is read: stdin is never touched.
	read := &readCounter{}
	stdin = read
	t.Cleanup(func() { stdin = os.Stdin })
	var errb strings.Builder
	if code := Main(context.Background(), []string{"admin", "bootstrap", "--secret-stdin"}, io.Discard, &errb, ClientFromEnv(func(k string) string { return env[k] })); code != entry.ExitUsage || read.n != 0 || !strings.Contains(errb.String(), client.ErrRemoteBootstrap.Error()) {
		t.Fatalf("%d, %d reads: %s", code, read.n, errb.String())
	}
}

type readCounter struct{ n int }

func (r *readCounter) Read([]byte) (int, error) { r.n++; return 0, io.EOF }
