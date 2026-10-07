// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

func id(t *testing.T, prefix string) string {
	t.Helper()
	s, err := ids.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// answering serves status and body to every request, recording its path
// and bearer.
func answering(t *testing.T, status int, ctype, body string, path, auth *string) Transport {
	socket := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*path, *auth = r.URL.RequestURI(), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", ctype)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	return transport(t, socket, os.Getuid())
}

// A read is returned only once the list, its resource and every record
// validate; a refusal only as a registered result, never quoting the body.
func TestRead(t *testing.T) {
	note := `{"id":"` + id(t, "not") + `","project":"` + id(t, "prj") + `","subject":null,"author":"` + id(t, "act") +
		`","body":"a\u202eb","created_at":"2026-10-07T00:00:00Z"}`
	list := `{"resource":"notes","outcome":"found","items":[` + note + `]}`
	var path, auth string
	for _, c := range []struct {
		resource, body string
		ok             bool
	}{
		{"notes", list, true},
		{"notes", strings.Replace(list, `"subject"`, `"zz":1,"subject"`, 1), false}, // a record its schema refuses
		{"notes", strings.Replace(list, `"notes"`, `"grants"`, 1), false},           // another resource
		{"notes", `{"resource":"notes","outcome":"found"}`, false},
		{"grants", strings.Replace(list, `"notes"`, `"grants"`, 1), false}, // a note is no grant
		{Whoami, `{"actor":"` + id(t, "act") + `","kind":"operator","token":"` + id(t, "tok") + `","listener":"private_socket","reads":{"everything":true,"grants":[]}}`, true},
		{Whoami, `{"actor":"x"}`, false},
	} {
		raw, err := answering(t, http.StatusOK, "application/json", c.body, &path, &auth).Read(context.Background(), c.resource)
		if c.ok != (err == nil) || (c.ok && string(raw) != c.body) || (!c.ok && !errors.Is(err, ErrInvalidRead)) {
			t.Errorf("%s %s: %v", c.resource, c.body, err)
		}
		if path != "/v1/"+c.resource || !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("sent %s %q", path, auth)
		}
	}
	path = ""
	if _, err := answering(t, http.StatusOK, "", list, &path, &auth).Read(context.Background(), "../commands"); !errors.Is(err, ErrNoResource) || path != "" {
		t.Errorf("an unregistered resource: %v, sent %q", err, path)
	}
	refused := `{"outcome":"failed","reason":"action_not_granted","detail":"no grant"}`
	for _, c := range []struct {
		status int
		body   string
		want   error
	}{
		{http.StatusUnauthorized, "", ErrUnauthenticated},
		{http.StatusBadRequest, "", ErrBadRequest},
		{http.StatusNotFound, "", ErrNoResource},
	} {
		if _, err := answering(t, c.status, "text/plain", c.body, &path, &auth).Read(context.Background(), "seat"); !errors.Is(err, c.want) {
			t.Errorf("HTTP %d: %v", c.status, err)
		}
	}
	var r *Refused
	if _, err := answering(t, http.StatusForbidden, "application/json", refused, &path, &auth).Read(context.Background(), "seat"); !errors.As(err, &r) || r.Result.Reason != vocab.ReasonActionNotGranted {
		t.Errorf("refused: %v", err)
	}
	for _, body := range []string{"zzQUOTEDzz", strings.Replace(refused, "failed", "found", 1), `{"outcome":"empty","reason":"completed_clean","detail":"d"}`,
		`{"outcome":"failed","reason":"store_unreadable","detail":"d"}`, `{"outcome":"failed","reason":"action_not_granted","detail":""}`} {
		_, err := answering(t, http.StatusServiceUnavailable, "text/plain", body, &path, &auth).Read(context.Background(), "seat")
		if err == nil || errors.As(err, &r) || strings.Contains(err.Error(), "zzQUOTEDzz") {
			t.Errorf("%s: %v", body, err)
		}
	}
	if _, err := transport(t, serve(t, http.NotFoundHandler()), os.Getuid()+1).Read(context.Background(), "seat"); !errors.Is(err, ErrPeerNotDaemon) {
		t.Errorf("another uid: %v", err)
	}
}
