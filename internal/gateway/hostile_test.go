// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
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
// The statuses and reasons are the case's expect.gateway_requests.
func TestHostileModelSwitch(t *testing.T) {
	const dir = "../../fixtures/m1/cases/hostile-model-switch/"
	var c struct {
		Expect struct {
			GatewayRequests []struct {
				File   string `json:"file"`
				Status int    `json:"status"`
				Reason string `json:"reason"`
			} `json:"gateway_requests"`
		} `json:"expect"`
	}
	raw, err := os.ReadFile(dir + "case.json")
	if err == nil {
		err = json.Unmarshal(raw, &c)
	}
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(dir + name)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	// The case's table (P22a's driver sends the same): file → status, reason.
	want := map[string]struct {
		status int
		reason string
	}{}
	for _, r := range c.Expect.GatewayRequests {
		want[r.File] = struct {
			status int
			reason string
		}{r.Status, r.Reason}
	}
	const leasedF, otherF, destF = "requests/leased.json", "requests/another-model.json", "requests/another-destination.txt"
	if len(want) != 3 || want[leasedF].status != http.StatusOK || want[destF].status != http.StatusOK {
		t.Fatalf("the case's gateway_requests: %+v", c.Expect.GatewayRequests)
	}
	leased, other, dest := read(leasedF), read(otherF), read(destF)
	up, elsewhere := newUpstream(t, nil), newUpstream(t, nil)
	g, token := rig(t, up, nil)
	if w := call(g, token, "/chat/completions", leased, nil); w.Code != want[leasedF].status || len(up.got()) != 1 {
		t.Fatalf("the leased model (control): %d %s, %d upstream requests", w.Code, w.Body, len(up.got()))
	}
	if w := call(g, token, "/chat/completions", other, nil); w.Code != want[otherF].status || want[otherF].reason == "" ||
		!strings.Contains(w.Body.String(), want[otherF].reason) || len(up.got()) != 1 {
		t.Errorf("another model: %d %s, %d upstream requests; the case wants %+v", w.Code, w.Body, len(up.got()), want[otherF])
	}
	e, err := url.Parse(elsewhere.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{dest, strings.Replace(dest, "198.51.100.7", e.Host, 1)} {
		r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(leased))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		g.ServeHTTP(deadlined{w}, r)
		if n := len(elsewhere.got()); w.Code != want[destF].status || n != 0 {
			t.Errorf("%s: %d %s, %d requests to another origin", target, w.Code, w.Body, n)
		}
	}
	if n := len(up.got()); n != 3 {
		t.Errorf("the leased destination got %d requests, want 3", n)
	}
}
