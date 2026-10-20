// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// P24c, §2.7 hostile-model-switch: the case's own requests on the
// attempt's gateway route. The leased model reaches the stub (the control);
// another model is refused before egress (model_not_pinned); a request
// naming another origin reaches only the leased destination: the gateway
// takes no destination from a request (the case states this deviation).
func TestHostileModelSwitch(t *testing.T) {
	dir := "../../fixtures/m1/cases/hostile-model-switch/requests/"
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(dir + name)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	leased, other, dest := read("leased.json"), read("another-model.json"), read("another-destination.txt")
	up, elsewhere := newUpstream(t, nil), newUpstream(t, nil)
	g, token := rig(t, up, nil)
	if w := call(g, token, "/chat/completions", leased, nil); w.Code != http.StatusOK || len(up.got()) != 1 {
		t.Fatalf("the leased model (control): %d %s, %d upstream requests", w.Code, w.Body, len(up.got()))
	}
	if w := call(g, token, "/chat/completions", other, nil); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "model_not_pinned") || len(up.got()) != 1 {
		t.Errorf("another model: %d %s, %d upstream requests", w.Code, w.Body, len(up.got()))
	}
	e, err := url.Parse(elsewhere.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{dest, strings.Replace(dest, "198.51.100.7", e.Host, 1)} {
		r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(leased))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if n := len(elsewhere.got()); w.Code != http.StatusOK || n != 0 {
			t.Errorf("%s: %d %s, %d requests to another origin", target, w.Code, w.Body, n)
		}
	}
	if n := len(up.got()); n != 3 {
		t.Errorf("the leased destination got %d requests, want 3", n)
	}
}
