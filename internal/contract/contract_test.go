// SPDX-License-Identifier: Apache-2.0

package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ekaynac/auto-dev-cycle/schemas"
)

// docSchema is a test-only contract over the common definitions, so the
// runtime is proven without depending on any record schema.
const docSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://github.com/ekaynac/auto-dev-cycle/schemas/test/doc.json",
  "type": "object",
  "additionalProperties": false,
  "required": ["id", "at", "n", "note"],
  "properties": {
    "id": {"$ref": "../common/defs.json#/$defs/ApprovalID"},
    "at": {"$ref": "../common/defs.json#/$defs/Time"},
    "ttl": {"$ref": "../common/defs.json#/$defs/Duration"},
    "n": {"type": "integer"},
    "note": {"type": "string"},
    "state": {
      "type": "object",
      "additionalProperties": false,
      "properties": {"version": {"type": "integer"}, "digest": {"$ref": "../common/defs.json#/$defs/Digest"}},
      "minProperties": 1,
      "maxProperties": 1
    },
    "scope": {"type": "object"},
    "why": {"$ref": "../common/defs.json#/$defs/ReasonCode"}
  }
}`

// testRegistry compiles the embedded common definitions plus docSchema.
func testRegistry(t *testing.T) registry {
	t.Helper()
	defs, err := fs.ReadFile(schemas.FS(), "common/defs.json")
	if err != nil {
		t.Fatal(err)
	}
	r, err := compile(fstest.MapFS{
		"common/defs.json": {Data: defs},
		"test/doc.json":    {Data: []byte(docSchema)},
	}, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return r
}

// typeID encodes 16 bytes as a TypeID, independently of CheckTypeID.
func typeID(prefix string, b [16]byte) string {
	n := new(big.Int).SetBytes(b[:])
	out := make([]byte, 26)
	for i := 25; i >= 0; i-- {
		m := new(big.Int)
		n.DivMod(n, big.NewInt(32), m)
		out[i] = base32[m.Int64()]
	}
	return prefix + "_" + string(out)
}

// uuid returns bytes with the given version nibble and variant bits.
func uuid(version, variant byte) [16]byte {
	b := [16]byte{0x01, 0x8f, 0x3a, 0x10, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd}
	b[6] = version<<4 | b[6]&0x0f
	b[8] = variant<<6 | b[8]&0x3f
	return b
}

var aprV7 = typeID("apr", uuid(7, 2))

type doc struct {
	ID    string          `json:"id"`
	At    string          `json:"at"`
	TTL   string          `json:"ttl,omitempty"`
	N     int64           `json:"n"`
	Note  string          `json:"note"`
	State *state          `json:"state,omitempty"`
	Scope json.RawMessage `json:"scope,omitempty"`
}

type state struct {
	Version *int64  `json:"version,omitempty"`
	Digest  *string `json:"digest,omitempty"`
}

func valid() string {
	return `{"id":"` + aprV7 + `","at":"2026-10-03T10:00:00Z","n":3,"note":"r","state":{"version":3},"scope":{"k":1}}`
}

func contractError(t *testing.T, err error, want string) {
	t.Helper()
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("want a contract error containing %q, got %v", want, err)
	}
	if !strings.Contains(ce.Detail, want) {
		t.Fatalf("want detail containing %q, got %q", want, ce.Detail)
	}
}

func TestEmbeddedSchemasCompile(t *testing.T) {
	r, err := load(false)
	if err != nil {
		t.Fatalf("embedded schemas: %v", err)
	}
	if r["common/defs"] == nil {
		t.Error("common/defs not compiled")
	}
}

func TestDecodeValid(t *testing.T) {
	d, err := decode[doc](testRegistry(t), "test/doc", []byte(valid()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.State == nil || d.State.Version == nil || *d.State.Version != 3 || string(d.Scope) != `{"k":1}` {
		t.Errorf("decoded %+v", d)
	}
}

func TestScalarFormats(t *testing.T) {
	r := testRegistry(t)
	set := func(field, value string) string {
		var m map[string]any
		_ = json.Unmarshal([]byte(valid()), &m)
		m[field] = value
		b, _ := json.Marshal(m)
		return string(b)
	}
	cases := []struct {
		name, field, value string
		ok                 bool
	}{
		{"time", "at", "2026-10-03T10:00:00Z", true},
		{"time with nanoseconds", "at", "2026-10-03T10:00:00.123456789Z", true},
		{"time out of range", "at", "2026-99-99T99:99:99Z", false},
		{"time on a day the month lacks", "at", "2026-02-30T00:00:00Z", false},
		{"time with an offset", "at", "2026-10-03T10:00:00+03:00", false},
		{"time with ten fraction digits", "at", "2026-10-03T10:00:00.1234567890Z", false},
		{"time as a word", "at", "banana", false},
		{"duration", "ttl", "PT1H30M", true},
		{"duration in days", "ttl", "P1DT2H", true},
		{"empty duration", "ttl", "PT", false},
		{"id: UUIDv7, right prefix", "id", aprV7, true},
		{"id: wrong entity prefix", "id", typeID("cal", uuid(7, 2)), false},
		{"id: UUIDv4", "id", typeID("apr", uuid(4, 2)), false},
		{"id: non-RFC variant", "id", typeID("apr", uuid(7, 3)), false},
		{"id: not base32", "id", "apr_01h455vb4pex5vsknk084sn0il", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := r.validate("test/doc", []byte(set(c.field, c.value)))
			if c.ok && err != nil {
				t.Fatalf("want valid, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatalf("%s=%q accepted", c.field, c.value)
			}
		})
	}
}

func TestDecodeRefuses(t *testing.T) {
	v := valid()
	cases := []struct{ name, doc, want string }{
		{"schema violation", strings.Replace(v, `"n":3`, `"n":"3"`, 1), "n"},
		{"trailing data", v + ` {}`, "trailing data"},
		{"non-integral spelling", strings.Replace(v, `"n":3`, `"n":3.0`, 1), "3.0"},
		{"exponent spelling", strings.Replace(v, `"n":3`, `"n":3e0`, 1), "3e0"},
		{"negative zero", strings.Replace(v, `"n":3`, `"n":-0`, 1), "-0"},
		{"not JSON", `{"id":`, "not JSON"},
		{"truncated", `{"id":"x"`, "not JSON"},
		// encoding/json keeps the last duplicate; JCS refuses it. Refuse at the boundary.
		{"duplicate key", strings.Replace(v, `"n":3`, `"n":3,"n":4`, 1), `duplicate object key "n"`},
		{"nested duplicate key", strings.Replace(v, `{"k":1}`, `{"k":1,"k":2}`, 1), `duplicate object key "k"`},
		{"duplicate key in an array element", `[{"a":1,"a":1}]`, `duplicate object key "a"`},
		{"invalid UTF-8", strings.Replace(v, `"note":"r"`, "\"note\":\"r\xff\"", 1), "invalid UTF-8"},
		{"lone high surrogate", strings.Replace(v, `"note":"r"`, `"note":"\ud800"`, 1), "unpaired surrogate"},
		{"lone low surrogate", strings.Replace(v, `"note":"r"`, `"note":"\udc00"`, 1), "unpaired surrogate"},
		{"high surrogate before a non-surrogate", strings.Replace(v, `"note":"r"`, `"note":"\ud800\u0041"`, 1), "unpaired surrogate"},
		{"surrogate in a key", strings.Replace(v, `"scope":{"k":1}`, `"scope":{"\udfff":1}`, 1), "unpaired surrogate"},
		{"too deep", strings.Repeat("[", 10002) + strings.Repeat("]", 10002), "max depth"},
	}
	r := testRegistry(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := decode[doc](r, "test/doc", []byte(c.doc))
			contractError(t, err, c.want)
		})
	}
}

// Characters that must survive unchanged: a surrogate pair, U+FFFD written
// raw and escaped, and an escaped backslash before "ud800".
func TestDecodeKeepsValidUnicode(t *testing.T) {
	r := testRegistry(t)
	for in, want := range map[string]string{
		`\ud83d\ude00`: "\U0001F600",
		"\uFFFD":       "\uFFFD",
		`\ufffd`:       "\uFFFD",
		`\\ud800`:      `\ud800`,
	} {
		d, err := decode[doc](r, "test/doc", []byte(strings.Replace(valid(), `"note":"r"`, `"note":"`+in+`"`, 1)))
		if err != nil || d.Note != want {
			t.Errorf("note %s: got %q, %v; want %q", in, d.Note, err, want)
		}
	}
}

// A valid document that the Go type cannot hold exactly is an error, never a
// silent drop: the type misses most fields.
func TestDecodeRefusesFieldsTheTypeLacks(t *testing.T) {
	type partial struct {
		ID string `json:"id"`
	}
	_, err := decode[partial](testRegistry(t), "test/doc", []byte(valid()))
	contractError(t, err, "does not fit the Go type")
}

// strictScope decodes itself through UnmarshalStrict, as generated union
// decoders do; laxScope uses json.Unmarshal and silently drops.
type strictScope struct {
	K int64 `json:"k"`
}

func (s *strictScope) UnmarshalJSON(b []byte) error {
	type plain strictScope
	return UnmarshalStrict(b, (*plain)(s))
}

type laxScope struct {
	K int64 `json:"k"`
}

func (s *laxScope) UnmarshalJSON(b []byte) error {
	type plain laxScope
	return json.Unmarshal(b, (*plain)(s))
}

func TestStrictnessInsideCustomDecoders(t *testing.T) {
	r := testRegistry(t)
	in := []byte(strings.Replace(valid(), `{"k":1}`, `{"k":1,"extra":2}`, 1)) // the schema allows extra
	type withStrict struct {
		doc
		Scope strictScope `json:"scope"`
	}
	_, err := decode[withStrict](r, "test/doc", in)
	contractError(t, err, `unknown field "extra"`)
	type withLax struct {
		doc
		Scope laxScope `json:"scope"`
	}
	if _, err := decode[withLax](r, "test/doc", in); err != nil {
		t.Fatalf("the documented limit changed: a lax custom decoder now fails: %v", err)
	}
}

func TestUnknownSchema(t *testing.T) {
	contractError(t, Validate("records/nope", []byte(`{}`)), "unknown schema")
}

func TestEncode(t *testing.T) {
	r := testRegistry(t)
	d, err := decode[doc](r, "test/doc", []byte(valid()))
	if err != nil {
		t.Fatal(err)
	}
	d.Note = "<a & b> \uFFFD " + `\ufffd`
	out, err := r.encode("test/doc", d)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(out), `"note":"<a & b> `+"\uFFFD"+` \\ufffd"`) || strings.HasSuffix(string(out), "\n") {
		t.Errorf("encode escaped HTML, changed U+FFFD or kept the newline: %s", out)
	}
	d.Note = "r\xff"
	_, err = r.encode("test/doc", map[string]any{"k\xff": 1})
	contractError(t, err, "invalid UTF-8")
	_, err = r.encode("test/doc", d)
	contractError(t, err, "invalid UTF-8")
	d.Note = "r"
	dg := "sha256:" + strings.Repeat("b", 64)
	d.State.Digest = &dg // both keys: maxProperties forbids it
	_, err = r.encode("test/doc", d)
	contractError(t, err, "state")
}

// badText encodes as text that is not valid UTF-8.
type badText struct{}

func (badText) MarshalText() ([]byte, error) { return []byte("r\xff"), nil }

type noteHolder struct {
	Note string `json:"note"`
}

// node points back at itself through a field the encoder ignores.
type node struct {
	doc
	Parent *node  `json:"-"`
	Hidden string `json:"-"`
}

// Encode follows the encoder's own field rules: promoted fields of an
// unexported embedded struct are written (and checked), ignored fields are
// neither written nor checked nor followed, and text from a MarshalText
// method is checked.
func TestEncodeChecksWhatTheEncoderWrites(t *testing.T) {
	r := testRegistry(t)
	base, err := decode[doc](r, "test/doc", []byte(valid()))
	if err != nil {
		t.Fatal(err)
	}
	type promoted struct {
		ID string `json:"id"`
		At string `json:"at"`
		N  int64  `json:"n"`
		noteHolder
	}
	_, err = r.encode("test/doc", promoted{ID: base.ID, At: base.At, N: 1, noteHolder: noteHolder{Note: "r\xff"}})
	contractError(t, err, "invalid UTF-8")

	type textNote struct {
		ID   string  `json:"id"`
		At   string  `json:"at"`
		N    int64   `json:"n"`
		Note badText `json:"note"`
	}
	_, err = r.encode("test/doc", textNote{ID: base.ID, At: base.At, N: 1})
	contractError(t, err, "invalid UTF-8")

	n := &node{doc: base, Hidden: "r\xff"}
	n.Parent = n
	if _, err := r.encode("test/doc", n); err != nil {
		t.Fatalf("ignored fields were checked or followed: %v", err)
	}
}

// Reason fields are checked by the installed vocabulary; with none
// installed, every reason is refused (fail closed).
func TestReasonFieldsNeedTheVocabulary(t *testing.T) {
	r := testRegistry(t)
	doc := func(why string) []byte {
		return []byte(strings.Replace(valid(), `"note":"r"`, `"note":"r","why":"`+why+`"`, 1))
	}
	t.Cleanup(func() { reasonCheck.Store(nil) })
	reasonCheck.Store(nil)
	contractError(t, r.validate("test/doc", doc("known")), "no reason vocabulary is loaded")
	stub := func(code string, writing bool) error {
		if code != "known" {
			return fmt.Errorf("unregistered reason %q", code)
		}
		return nil
	}
	reasonCheck.Store(&stub)
	if err := r.validate("test/doc", doc("known")); err != nil {
		t.Fatalf("a registered reason: %v", err)
	}
	contractError(t, r.validate("test/doc", doc("typo")), `unregistered reason "typo"`)
}

// Only the vocabulary package installs the reason check: a caller here (the
// contract package's own test) is refused, and nothing is installed.
func TestSetReasonCheckRefusesOtherCallers(t *testing.T) {
	t.Cleanup(func() { reasonCheck.Store(nil) })
	reasonCheck.Store(nil)
	if err := SetReasonCheck(func(string, bool) error { return nil }); err == nil {
		t.Fatal("a caller outside the vocabulary installed the reason check")
	}
	if reasonCheck.Load() != nil {
		t.Fatal("a refused installation left a check behind")
	}
}
