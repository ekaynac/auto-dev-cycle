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
