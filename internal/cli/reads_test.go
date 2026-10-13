// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// reader is a Client that reads: its reads answer from body or err.
type reader struct {
	fake
	body     string
	err      error
	resource string
}

func (r *reader) Read(_ context.Context, resource string) ([]byte, error) {
	r.resource = resource
	return []byte(r.body), r.err
}

func run2(c Client, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(context.Background(), args, &out, &errb, c)
	return code, out.String(), errb.String()
}

func refusal(o vocab.Outcome, reason vocab.Reason) error {
	return &client.Refused{Result: vocab.Result{Outcome: o, Reason: reason, Detail: "d\x1b[2J"}}
}

// Each read command reads its resource; --json prints the body byte for
// byte (untrusted text included), human output escapes it.
func TestReadCommands(t *testing.T) {
	for args, resource := range map[string]string{"grant list": "grants", "propose list": "proposals", "seat status": "seat",
		"notes list": "notes", "project list": "projects", "admin whoami": client.Whoami} {
		r := &reader{body: `{"resource":"x","outcome":"empty","items":[]}`}
		if code, _, errb := run2(r, append(strings.Fields(args), "--json")...); code != entry.ExitOK || r.resource != resource {
			t.Errorf("%s: %d %q %s", args, code, r.resource, errb)
		}
	}
	body := `{"resource":"notes","outcome":"found","items":[{"id":"not_1","body":"a\u202eb\nnotes not_2\n\\u{1}","subject":null,"author":"x\u2067y"}]}`
	r := &reader{body: body + "\n"}
	if code, out, _ := run2(r, "notes", "list", "--json"); code != entry.ExitOK || out != body+"\n" {
		t.Fatalf("json: %d %q", code, out)
	}
	code, out, _ := run2(r, "notes", "list")
	want := "notes not_1\n  author: x\\u{2067}y\n  body:\n    | a\\u{202E}b\n    | notes not_2\n    | \\\\u{1}\n  id: not_1\n  subject: -\n"
	if code != entry.ExitOK || out != want {
		t.Fatalf("human: %d\n%s", code, out)
	}
	r.body = `{"resource":"grants","outcome":"empty","items":[]}`
	if _, out, _ := run2(r, "grant", "list"); out != "grants: none readable\n" {
		t.Errorf("empty: %q", out)
	}
	r.body = `{"actor":"act_1","kind":"operator","reads":{"everything":true}}`
	if _, out, _ := run2(r, "admin", "whoami"); !strings.HasPrefix(out, "whoami act_1\n  actor: act_1\n") || !strings.Contains(out, `  reads: {"everything":true}`) {
		t.Errorf("whoami: %q", out)
	}
}

// Every refusal maps to the exit-code table; nothing of the controller's
// text reaches the terminal unescaped.
func TestReadExitCodes(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
	}{
		{refusal(vocab.Failed, vocab.ReasonActionNotGranted), entry.ExitRefused},
		{refusal(vocab.CouldNotRun, vocab.ReasonStoreUnreadable), entry.ExitCouldNotRun},
		{client.ErrUnauthenticated, entry.ExitRefused},
		{client.ErrBadRequest, entry.ExitUsage},
		{errors.New("dial\x1b[2J"), entry.ExitCouldNotRun},
	} {
		code, out, errb := run2(&reader{err: c.err}, "seat", "status")
		if code != c.code || out != "" || strings.Contains(errb, "\x1b") {
			t.Errorf("%v: %d %q %q", c.err, code, out, errb)
		}
	}
	if code, _, _ := run2(&fake{}, "seat", "status"); code != entry.ExitCouldNotRun {
		t.Errorf("no transport: %d", code)
	}
	for _, args := range [][]string{{"seat", "status", "--bogus"}, {"notes", "list", "extra"}} {
		if code, _, _ := run2(&reader{}, args...); code != entry.ExitUsage {
			t.Errorf("%v: %d", args, code)
		}
	}
	// The flag package prints nothing itself: an unknown flag is reported
	// escaped, and -h prints the command's own flags.
	if code, out, errb := run2(&reader{}, "seat", "status", "--x\x1b[2J"); code != entry.ExitUsage || out != "" ||
		errb != "adc seat status: flag provided but not defined: -x\\u{001B}[2J\n" {
		t.Errorf("unknown flag: %d %q", code, errb)
	}
	if code, _, errb := run2(&reader{}, "seat", "status", "-h"); code != entry.ExitOK || !strings.Contains(errb, "-json") {
		t.Errorf("-h: %d %q", code, errb)
	}
}

// safe escapes every class of rune that could move, hide or restyle text.
func TestSafe(t *testing.T) {
	for in, want := range map[string]string{
		"plain ünïcode 日本": "plain ünïcode 日本",
		"\x1b[31mred":      `\u{001B}[31mred`, // a terminal escape
		"\u009b31m":        `\u{009B}31m`,     // a C1 control
		"a\u202eb\u2066c":  `a\u{202E}b\u{2066}c`,
		"a\u200bb\ufeffc":  `a\u{200B}b\u{FEFF}c`,
		"a\u3164b":         `a\u{3164}b`, // invisible filler
		"a\u2028b":         `a\u{2028}b`,
		"a\xffb":           `a\xffb`,
		`a\u{202E}b`:       `a\\u{202E}b`,
		"tab\there":        `tab\u{0009}here`,
		"a\u009fb":         `a\u{009F}b`,          // the last C1 control
		"a\ufe0fb\u180bc":  `a\u{FE0F}b\u{180B}c`, // variation selectors
		"a\u2800b":         `a\u{2800}b`,          // blank braille
		"\u0301x\u20dd":    "\\u{0301}x\u20dd",    // a leading combining mark, not a later one
		"\u20ddx":          `\u{20DD}x`,           // a leading enclosing mark
	} {
		if got := safe(in); got != want {
			t.Errorf("safe(%q) = %q, want %q", in, got, want)
		}
	}
}

// hostile is one value of every kind the terminal must not take raw: a
// CSI sequence, a C1 CSI, DEL, a bidi override and isolate, a zero-width
// space, a line separator and an invalid byte.
const hostile = "\x1b[2J\u009b31m\x7f\u202e\u2066\u200b\u2028\xff"

// raw reports whether s carries any rune of hostile unescaped.
func raw(s string) bool {
	return strings.ContainsAny(s, "\x1b\u009b\x7f\u202e\u2066\u200b\u2028") || !utf8.ValidString(s)
}

// A hostile value in every field the human output shows, read or refused,
// is escaped, on stdout and on stderr.
func TestHostileReadFields(t *testing.T) {
	h, _ := json.Marshal(hostile)
	for name, body := range map[string]string{
		"id":        `{"resource":"notes","outcome":"found","items":[{"id":` + string(h) + `}]}`,
		"value":     `{"resource":"notes","outcome":"found","items":[{"id":"n","body":` + string(h) + `}]}`,
		"block":     `{"resource":"notes","outcome":"found","items":[{"id":"n","body":` + strings.Replace(string(h), `\u2028`, `\n\u2028`, 1) + `}]}`,
		"block key": `{"resource":"notes","outcome":"found","items":[{"id":"n",` + string(h) + `:"a\nb"}]}`,
		"key":       `{"resource":"notes","outcome":"found","items":[{"id":"n",` + string(h) + `:1}]}`,
		"nested":    `{"resource":"notes","outcome":"found","items":[{"id":"n","object":{"id":` + string(h) + `}}]}`,
		"whoami":    `{"actor":` + string(h) + `}`,
	} {
		resource := []string{"notes", "list"}
		if name == "whoami" {
			resource = []string{"admin", "whoami"}
		}
		if code, out, errb := run2(&reader{body: body}, resource...); code != entry.ExitOK || raw(out) || raw(errb) || !strings.Contains(out, `\u{202E}`) {
			t.Errorf("%s: %d %q %q", name, code, out, errb)
		}
	}
	for name, err := range map[string]error{
		"outcome": &client.Refused{Result: vocab.Result{Outcome: hostile, Reason: vocab.ReasonActionNotGranted, Detail: "d"}},
		"detail":  &client.Refused{Result: vocab.Result{Outcome: vocab.Failed, Reason: vocab.ReasonActionNotGranted, Detail: hostile}},
		"error":   errors.New(hostile),
		"401":     fmt.Errorf("%w %s", client.ErrUnauthenticated, hostile),
		"400":     fmt.Errorf("%w %s", client.ErrBadRequest, hostile),
	} {
		if _, out, errb := run2(&reader{err: err}, "seat", "status"); raw(out) || raw(errb) || !strings.Contains(errb, `\u{202E}`) {
			t.Errorf("%s: %q %q", name, out, errb)
		}
	}
	if _, _, errb := run2(&reader{}, "seat", "status", hostile); raw(errb) || !strings.Contains(errb, `\u{202E}`) {
		t.Errorf("argument: %q", errb)
	}
	var errb bytes.Buffer
	if Main(context.Background(), []string{"seat", "status"}, failingWriter{hostile}, &errb, &reader{body: `{"resource":"seat","outcome":"empty","items":[]}`}) != entry.ExitCouldNotRun || raw(errb.String()) {
		t.Errorf("write error: %q", errb.String())
	}
}

// --json is the document as sent, a bidi rune included, but a DEL or C1
// control or an invalid byte goes out as its \u escape (the same value).
func TestJSONControls(t *testing.T) {
	r := &reader{body: "{\"resource\":\"notes\",\"items\":[{\"body\":\"a\u202e\u009b\x7f\xffb\"}]}\n"}
	_, out, _ := run2(r, "notes", "list", "--json")
	if out != "{\"resource\":\"notes\",\"items\":[{\"body\":\"a\u202e\\u009b\\u007f\\ufffdb\"}]}\n" {
		t.Fatalf("%q", out)
	}
	var doc struct{ Items []struct{ Body string } }
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Items[0].Body != "a\u202e\u009b\x7f\ufffdb" {
		t.Fatalf("%v %q", err, doc)
	}
	// Whitespace between tokens (CR, LF, tab) is compacted away, and line
	// and paragraph separators and the last C1 control are escaped.
	r = &reader{body: "{\r\n\t\"resource\" :\t\"notes\",\r\"items\":\n[{\"body\":\"a\u2028b\u2029c\u009fd\"}]}\r\n"}
	if _, out, _ := run2(r, "notes", "list", "--json"); out != `{"resource":"notes","items":[{"body":"a\u2028b\u2029c\u009fd"}]}`+"\n" {
		t.Fatalf("whitespace: %q", out)
	}
	// encoding/json refuses a raw C0 control in a string: a valid document has none.
	if json.Valid([]byte("\"a\x1bb\"")) {
		t.Fatal("a raw ESC is valid JSON")
	}
}

type failingWriter struct{ msg string }

func (f failingWriter) Write([]byte) (int, error) { return 0, errors.New(f.msg) }
