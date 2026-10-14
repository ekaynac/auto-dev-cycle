// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// spelled is key with byte i written as how(i, c) returns, raw for "".
func spelled(key string, how func(i int, c byte) string) string {
	var b strings.Builder
	for i := range len(key) {
		if s := how(i, key[i]); s != "" {
			b.WriteString(s)
		} else {
			b.WriteByte(key[i])
		}
	}
	return b.String()
}

// escaping writes the bytes in pick (all when "") as `\u` with format's hex.
func escaping(format, pick string) func(int, byte) string {
	return func(_ int, c byte) string {
		if pick == "" || strings.IndexByte(pick, c) >= 0 {
			return fmt.Sprintf(`\u`+format, c)
		}
		return ""
	}
}

// M1 (Opus on #256): any JSON spelling of the key is redacted, however an
// encoder escapes each byte, in one write or in single bytes.
func TestEveryJSONSpellingIsRedacted(t *testing.T) {
	const key = "wJalrXUtn+FEMI/K7MDENG=bPxRfiCY<>&q"
	letters := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	for name, body := range map[string]string{
		"'+' as .NET escapes it": spelled(key, escaping("%04X", "+")),
		"'=' as Gson escapes it": spelled(key, escaping("%04x", "=")),
		`'/' as /`:               spelled(key, escaping("%04x", "/")),
		`'/' as \/`:              strings.ReplaceAll(key, "/", `\/`),
		"only '<' escaped":       spelled(key, escaping("%04x", "<")),
		"mixed-case hex": spelled(key, func(i int, c byte) string {
			return fmt.Sprintf([]string{`\u%04x`, `\u%04X`}[i%2], c)
		}),
		"every letter escaped": spelled(key, escaping("%04x", letters)),
		"every byte escaped":   spelled(key, escaping("%04X", "")),
	} {
		for _, step := range []int{len(body), 1} {
			var out bytes.Buffer
			r := &redacting{w: &out, secret: []byte(key)}
			for i := 0; i < len(body); i += step {
				_, _ = r.Write([]byte(body[i:min(i+step, len(body))]))
			}
			_ = r.Close()
			if out.String() != redacted || !r.hit {
				t.Errorf("%s (writes of %d bytes): %q", name, step, out.String())
			}
		}
	}
	// Astra on #256, M1: a key with a backslash, raw or doubled, or a
	// non-ASCII byte, has spellings the matcher cannot follow byte by byte
	// (`\`, a rune's `é`): the gateway never holds or injects it.
	for _, key := range []string{`abc\def0123456789`, `abc\\def0123456789`, "kéy0123456789abc"} {
		up := newUpstream(t, nil)
		g, token := rig(t, up, func(c *Config) { c.Secrets = secrets{"stub": key} })
		if w := call(g, token, "/chat/completions", good, nil); w.Code != http.StatusServiceUnavailable ||
			w.Header().Get(RefusalHeader) != "credential_not_held" || len(up.got()) != 0 {
			t.Errorf("%q: %d %v, upstream saw %d", key, w.Code, w.Header(), len(up.got()))
		}
		if held, err := g.Holds(context.Background(), "stub", "stub"); held || err == nil || strings.Contains(err.Error(), key) {
			t.Errorf("%q: held %v, %v", key, held, err)
		}
	}
}

// L1 (Opus on #256): the EOF rule counts minTail bytes past the key's
// public prefix, so a stream cut at the prefix is no echo.
func TestATailOfThePublicPrefixIsNoEcho(t *testing.T) {
	const key = "sk-proj-0a1b2c3d4e5f6g7h8i9j"
	for tail, echo := range map[string]bool{"sk-proj-": false, "sk-proj-0a1b2c3": false, "sk-proj-0a1b2c3d": true} {
		var out bytes.Buffer
		r := &redacting{w: &out, secret: []byte(key)}
		_, _ = r.Write([]byte("data: " + tail))
		if _ = r.Close(); r.hit != echo || strings.Contains(out.String(), tail) == echo {
			t.Errorf("%q: %q, hit %v", tail, out.String(), r.hit)
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
