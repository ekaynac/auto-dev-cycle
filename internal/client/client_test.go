// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
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

func TestCommandResponses(t *testing.T) {
	refused := `{"disposition":"refused","outcome":"failed","reason":"seat_not_held","detail":"d"}`
	bodyBound := map[string]bool{"oversized": true, "endless": true} // refused by the bound itself
	with := func(status int, body string, header ...string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			for i := 0; i+1 < len(header); i += 2 {
				w.Header().Set(header[i], header[i+1])
			}
			w.WriteHeader(status)
			io.WriteString(w, body)
		}
	}
	for name, c := range map[string]struct {
		h  http.HandlerFunc
		ok bool
	}{
		"a refusal with a status":           {with(http.StatusConflict, refused), true},
		"a refusal with a 302 and Location": {with(http.StatusFound, refused, "Location", "/elsewhere"), true},
		"a refusal with a 307 and Location": {with(http.StatusTemporaryRedirect, refused, "Location", "/elsewhere"), true},
		"a bare redirect":                   {with(http.StatusTemporaryRedirect, "", "Location", "/elsewhere"), false},
		"an unknown disposition":            {with(http.StatusOK, `{"disposition":"deferred"}`), false},
		"a refusal without detail":          {with(http.StatusOK, `{"disposition":"refused","outcome":"failed","reason":"seat_not_held"}`), false},
		"an application without its fields": {with(http.StatusOK, `{"disposition":"applied","command":"cmd_01m3250vz2ezn81cgvbkcn17jd"}`), false},
		"an error page":                     {with(http.StatusInternalServerError, "no"), false},
		"oversized":                         {with(http.StatusOK, strings.Repeat(" ", maxResponseBytes)+applied), false},
		"endless": {func(w http.ResponseWriter, _ *http.Request) {
			for chunk := []byte(strings.Repeat(" ", 64<<10)); ; {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		}, false},
		"oversized headers": {with(http.StatusOK, applied, "X-Pad", strings.Repeat("a", maxHeaderBytes)), false},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			hits := 0
			socket := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				hits++
				mu.Unlock()
				c.h(w, r)
			}))
			// The body bound, not a timeout, ends an endless body.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := transport(t, socket, os.Getuid()).Command(ctx, "task.create", request(t))
			if (err == nil) != c.ok || bodyBound[name] && !strings.Contains(fmt.Sprint(err), "exceeds") {
				t.Fatalf("got %v", err)
			}
			noSecret(t, err)
			mu.Lock()
			defer mu.Unlock()
			if hits != 1 {
				t.Fatalf("%d requests: a redirect was followed", hits)
			}
		})
	}
}

// An invalid request is refused by the schema before anything is dialled
// or the token is read.
func TestCommandRefusesAnInvalidRequest(t *testing.T) {
	path := socketPath(t)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := l.Accept(); err == nil {
			accepted <- struct{}{}
			c.Close()
		}
	}()
	tokens := 0
	tr := Transport{Socket: path, DaemonUID: os.Getuid(), Token: func() (identity.Secret, error) { tokens++; return fakeToken(t), nil }}
	_, err = tr.Command(context.Background(), "task.create", []byte(`{"reason":""}`))
	var cerr *contract.Error
	if !errors.As(err, &cerr) || cerr.Schema != "api/command-request" {
		t.Fatalf("got %v", err)
	}
	l.Close()
	select {
	case <-accepted:
		t.Fatal("dialled")
	default:
	}
	if tokens != 0 {
		t.Fatal("the token was read")
	}
}

// The connection policy: no proxy (the environment's is ignored), no
// connection reuse, bounded headers, and timeouts on everything but a
// stream's body.
func TestConnectionPolicy(t *testing.T) {
	tr := Transport{}
	for _, stream := range []bool{false, true} {
		c := tr.httpClient(stream)
		ht := c.Transport.(*http.Transport)
		if ht.Proxy != nil || !ht.DisableKeepAlives || ht.MaxResponseHeaderBytes != maxHeaderBytes || ht.ResponseHeaderTimeout != headerTimeout || ht.DialContext == nil {
			t.Fatalf("stream %v: %+v", stream, ht)
		}
		if want := map[bool]time.Duration{false: commandTimeout, true: 0}[stream]; c.Timeout != want {
			t.Fatalf("stream %v: timeout %v", stream, c.Timeout)
		}
	}
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	socket := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, applied) }))
	if _, err := transport(t, socket, os.Getuid()).Command(context.Background(), "task.create", request(t)); err != nil {
		t.Fatalf("under a proxy environment: %v", err)
	}
}

func TestDecodeResponseFailsClosed(t *testing.T) {
	if r, err := DecodeResponse([]byte(applied)); err != nil || r.Variant == nil {
		t.Fatalf("applied: %v", err)
	}
	for _, raw := range []string{
		`{"disposition":"deferred","outcome":"failed","reason":"seat_not_held","detail":"d"}`,
		`{"disposition":"Applied","command":"cmd_01m3250vz2ezn81cgvbkcn17jd","replayed":false,"approval":null,"first_seq":4,"last_seq":4,"effects":[],"result":{}}`,
		`{"disposition":"refused","outcome":"failed","reason":"seat_not_held"}`,
		`{"disposition":"proposed","proposal":"prp_01m3250vm7f6ktw39dszqnyddr"}`,
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
