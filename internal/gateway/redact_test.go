// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// An AWS-style key, with the characters JSON encoders escape. A test value.
const escapable = "fake/key<0123>&456789"

// F (P11b): an encoded credential echo reaching the adapter. An upstream
// quoting the key through a JSON encoder that escapes '/' (PHP) or '<>&'
// (Go, or PHP's JSON_HEX_TAG), or cutting a stream inside it, never hands
// it, or more than minTail-1 bytes of it, to the adapter (#226 entries 55,
// 56).
func TestAnEscapedOrCutEchoNeverReachesTheAdapter(t *testing.T) {
	goStyle, _ := json.Marshal(escapable)
	for name, body := range map[string]string{
		"Go's escaping":            `{"error":` + string(goStyle) + `}`,
		"PHP's escaping":           `{"error":"fake\/key<0123>&456789"}`,
		"both, upper-case hex":     `{"error":"fake\/key<0123>&456789"}`,
		"a stream cut inside it":   "data: {\"x\":\"fake/key<012",
		"an escaped key cut short": `{"error":"fake\/key<01`,
	} {
		up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
		g, token := rig(t, up, func(c *Config) { c.Secrets = secrets{"stub": escapable} })
		got := call(g, token, "/chat/completions", good, nil).Body.String()
		for _, leak := range []string{"key<01", `key<01`, `key<01`, `fake\/key`, "fake/key"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: the adapter got %q", name, got)
			}
		}
		if o := g.Unbind(token); !strings.Contains(got, redacted) || len(o.Refusals) != 1 || o.Refusals[0] != RouteEchoed {
			t.Errorf("%s: %q, observed %+v", name, got, o)
		}
	}
}

// The forms scanned for: the raw value once when no character needs
// escaping, and every escaped spelling otherwise, longest first.
func TestTheFormsScannedFor(t *testing.T) {
	if f := forms([]byte(fakeKey)); len(f) != 1 || string(f[0]) != fakeKey {
		t.Fatalf("forms of a plain key: %q", f)
	}
	f := forms([]byte(escapable))
	for _, want := range []string{escapable, `fake\/key<0123>&456789`, `fake/key<0123>&456789`, `fake\/key<0123>&456789`} {
		if !bytes.Contains(bytes.Join(f, []byte("|")), []byte(want)) {
			t.Errorf("forms %q lack %q", f, want)
		}
	}
	for i := 1; i < len(f); i++ {
		if len(f[i]) > len(f[0]) {
			t.Fatalf("forms not longest first: %q", f)
		}
	}
}

// A Content-Type reaches the adapter only as a known media type, with a
// known charset: the media type or charset carrying the key is dropped,
// never scanned (#226 entry 56).
func TestOnlyAKnownContentTypeIsForwarded(t *testing.T) {
	for header, want := range map[string]string{
		"application/json; charset=UTF-8":                       "application/json; charset=utf-8",
		"text/event-stream; charset=" + fakeKey:                 "text/event-stream",
		"application/x-" + fakeKey:                              "",
		`text/plain; charset="utf-8"; leak=` + fakeKey:          "text/plain; charset=utf-8",
		"text/html; charset=iso-8859-1":                         "text/html; charset=iso-8859-1",
		"application/json; charset=" + strings.ToUpper(fakeKey): "application/json",
	} {
		up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("Content-Type", header) })
		g, token := rig(t, up, nil)
		if got := call(g, token, "/chat/completions", good, nil).Header().Get("Content-Type"); got != want {
			t.Errorf("%q forwarded as %q, want %q", header, got, want)
		}
	}
}
