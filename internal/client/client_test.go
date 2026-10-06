// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	_ "github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const applied = `{"disposition":"applied","command":"cmd_01m3250vz2ezn81cgvbkcn17jd","replayed":false,"approval":null,"first_seq":4,"last_seq":4,"effects":[],"result":{}}`

func request(t *testing.T) []byte {
	t.Helper()
	v := int64(0)
	b, err := EncodeRequest(types.CommandRequest{IdempotencyKey: "k", Expected: types.ExpectedState{Version: &v}, Scope: json.RawMessage(`{}`), Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func socketPath(t *testing.T) string {
	t.Helper()
	// Unix socket paths are short; t.TempDir may be too deep.
	dir, err := os.MkdirTemp("", "adc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "adcd.sock")
}

// serve runs h on a Unix socket and returns its path.
func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	path := socketPath(t)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return path
}

func transport(t *testing.T, socket string, uid int) Transport {
	tok := fakeToken(t)
	return Transport{Socket: socket, DaemonUID: uid, Token: func() (identity.Secret, error) { return tok, nil }}
}

// A peer of another uid gets no byte at all: the check runs on the
// connection before the request (and its bearer) is written.
func TestPeerOfAnotherUIDGetsNothing(t *testing.T) {
	path := socketPath(t)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var mu sync.Mutex
	var got []byte
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c)
		mu.Lock()
		got = b
		mu.Unlock()
	}()
	_, err = transport(t, path, os.Getuid()+1).Command(context.Background(), "task.create", request(t))
	if !errors.Is(err, ErrPeerNotDaemon) {
		t.Fatalf("got %v", err)
	}
	noSecret(t, err)
	<-accepted
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("the server received %d bytes", len(got))
	}
}

func TestCommand(t *testing.T) {
	var auth, path, ctype string
	socket := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path, ctype = r.Header.Get("Authorization"), r.URL.Path, r.Header.Get("Content-Type")
		io.WriteString(w, applied)
	}))
	tr := transport(t, socket, os.Getuid())
	raw, err := tr.Command(context.Background(), "task.create", request(t))
	if err != nil || string(raw) != applied {
		t.Fatalf("%s %v", raw, err)
	}
	tok, _ := tr.Token()
	if auth != "Bearer "+tok.Reveal() || path != "/v1/commands/task.create" || ctype != "application/json" {
		t.Fatalf("sent %q %q %q", auth, path, ctype)
	}
}

func TestCommandRefusals(t *testing.T) {
	refused := `{"disposition":"refused","outcome":"failed","reason":"seat_not_held","detail":"d"}`
	var redirected bool
	for name, c := range map[string]struct {
		h  http.HandlerFunc
		ok bool
	}{
		"a refusal with a status": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, refused)
		}, true},
		"an unknown disposition": {func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, `{"disposition":"deferred"}`)
		}, false},
		"an error page": {func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", http.StatusInternalServerError)
		}, false},
		"oversized": {func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, strings.Repeat(" ", maxResponseBytes)+applied)
		}, false},
		"a redirect": {func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/elsewhere" {
				redirected = true
				return
			}
			http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := transport(t, serve(t, c.h), os.Getuid()).Command(context.Background(), "task.create", request(t))
			if (err == nil) != c.ok {
				t.Fatalf("got %v", err)
			}
			noSecret(t, err)
		})
	}
	if redirected {
		t.Fatal("a redirect was followed")
	}
}

// An invalid request is refused before anything is dialled.
func TestCommandRefusesAnInvalidRequest(t *testing.T) {
	tr := transport(t, "/nonexistent/adcd.sock", os.Getuid())
	if _, err := tr.Command(context.Background(), "task.create", []byte(`{"reason":""}`)); err == nil || errors.Is(err, ErrPeerNotDaemon) {
		t.Fatalf("got %v", err)
	}
}

func TestDecodeResponseFailsClosed(t *testing.T) {
	if r, err := DecodeResponse([]byte(applied)); err != nil || r.Variant == nil {
		t.Fatalf("applied: %v", err)
	}
	for _, raw := range []string{
		`{"disposition":"deferred","outcome":"failed","reason":"seat_not_held","detail":"d"}`,
		`{"disposition":"Applied","command":"cmd_01m3250vz2ezn81cgvbkcn17jd","replayed":false,"approval":null,"first_seq":4,"last_seq":4,"effects":[],"result":{}}`,
		`{}`,
		`null`,
	} {
		if _, err := DecodeResponse([]byte(raw)); err == nil {
			t.Errorf("decoded %s", raw)
		}
	}
}

// A stream stays open on its verified connection and carries the bearer
// like a command.
func TestStream(t *testing.T) {
	var auth string
	socket := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "id: 1\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	tr := transport(t, socket, os.Getuid())
	body, err := tr.Stream(context.Background(), "/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	line, err := bufio.NewReader(body).ReadString('\n')
	if err != nil || line != "id: 1\n" || !strings.HasPrefix(auth, "Bearer ") {
		t.Fatalf("%q %v %q", line, err, auth)
	}
	if _, err := transport(t, socket, os.Getuid()+1).Stream(context.Background(), "/v1/events"); !errors.Is(err, ErrPeerNotDaemon) {
		t.Fatalf("another uid: %v", err)
	}
}

// secretPart is recognizable in any string it leaks into.
const secretPart = "zzFAKEzzSECRETzzRECOGNIZABLEzz" + "QQQQQQQQQQQQA"

// fakeToken is a bearer of the issued form whose secret is secretPart.
func fakeToken(t *testing.T) identity.Secret {
	t.Helper()
	id, err := ids.New("tok")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := identity.ParseSecret(id + "." + secretPart)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// noSecret fails when err quotes the fake token.
func noSecret(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), secretPart) {
		t.Fatalf("an error quotes the token: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
