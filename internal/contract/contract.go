// SPDX-License-Identifier: Apache-2.0

// Package contract validates, decodes and encodes contract documents against
// the embedded schemas (docs/design/m0/01 "Code generation", decoding and
// encoding contract):
//   - Decode validates the bytes, then decodes the same bytes strictly;
//   - trailing data after the document is refused;
//   - numbers must be spelled as integers (M0 contracts have no floats), and -0
//     is refused, so Go and TypeScript agree on identity;
//   - duplicate object keys (at any depth), invalid UTF-8 and unpaired
//     surrogate escapes are refused: encoding/json would silently keep the
//     last duplicate or substitute U+FFFD, while JCS (event hashes, input
//     digests) refuses them;
//   - Encode refuses to write any string that is not valid UTF-8, including
//     the output of MarshalText and MarshalJSON methods (encoding/json would
//     write U+FFFD in its place), and validates its own output. Its bytes are
//     not a canonical form (key order follows the Go struct): every digest is
//     taken over the JCS canonicalisation, never over Encode output;
//   - the adc-time, adc-typeid and adc-reason formats (schemas/common/defs.json)
//     are asserted: a calendar-valid UTC time, a TypeID over a UUIDv7, and a
//     reason code registered in the vocabulary (SetReasonCheck).
//
// Strict decoding reaches every type decoded by encoding/json's default
// rules. A type with its own UnmarshalJSON must decode its fields through
// UnmarshalStrict, as generated decoders do; Decode cannot see inside it.
package contract

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ekaynac/auto-dev-cycle/schemas"
)

// Error reports a document that does not satisfy its contract.
type Error struct {
	Schema string // schema path without .json, e.g. "common/defs"
	Detail string
}

func (e *Error) Error() string { return "contract " + e.Schema + ": " + e.Detail }

// registry is a compiled schema set, keyed by path without ".json".
type registry map[string]*jsonschema.Schema

var (
	loadOnce         sync.Once
	readers, writers registry
	loadErr          error
)

// load compiles the embedded schemas once per direction: readers accept
// every reason ever registered, writers refuse deprecated ones (note 02). A
// schema that does not compile is a build defect, reported on every call.
func load(writing bool) (registry, error) {
	loadOnce.Do(func() {
		if readers, loadErr = compile(schemas.FS(), false); loadErr == nil {
			writers, loadErr = compile(schemas.FS(), true)
		}
	})
	if writing {
		return writers, loadErr
	}
	return readers, loadErr
}

// compile builds a registry from every schema file in fsys; each file's $id
// is schemas.Base + its path, so cross-file $refs resolve inside fsys only.
func compile(fsys fs.FS, writing bool) (registry, error) {
	paths, err := schemas.PathsIn(fsys)
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	c.RegisterFormat(&jsonschema.Format{Name: "adc-time", Validate: onString(checkTime)})
	c.RegisterFormat(&jsonschema.Format{Name: "adc-typeid", Validate: onString(CheckTypeID)})
	c.RegisterFormat(&jsonschema.Format{Name: "adc-reason", Validate: onString(func(code string) error { return checkReason(code, writing) })})
	for _, p := range paths {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("schema %s: %w", p, err)
		}
		if err := c.AddResource(schemas.Base+p, doc); err != nil {
			return nil, fmt.Errorf("schema %s: %w", p, err)
		}
	}
	out := registry{}
	for _, p := range paths {
		s, err := c.Compile(schemas.Base + p)
		if err != nil {
			return nil, fmt.Errorf("schema %s: %w", p, err)
		}
		out[strings.TrimSuffix(p, ".json")] = s
	}
	return out, nil
}

// onString applies a string check; other types are not this format's concern
// (the schema's "type" keyword covers them).
func onString(check func(string) error) func(any) error {
	return func(v any) error {
		if s, ok := v.(string); ok {
			return check(s)
		}
		return nil
	}
}

// checkTime requires a real calendar date and clock time; the schema pattern
// already fixes the lexical form (UTC, Z suffix).
func checkTime(s string) error {
	if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
		return fmt.Errorf("not a valid time: %v", err)
	}
	return nil
}

// reasonCheck decides whether a reason code may appear in a document being
// read (registered) or written (registered and not deprecated).
// internal/vocab installs it; until then every reason is refused, so a
// document with a reason field can never pass unchecked.
var reasonCheck atomic.Pointer[func(code string, writing bool) error]

// reasonCheckOwner is the import path of the only package that may install
// the reason check.
const reasonCheckOwner = "github.com/ekaynac/auto-dev-cycle/internal/vocab"

// SetReasonCheck installs the vocabulary's reason check. Only the vocabulary
// package (exactly reasonCheckOwner) may call it, once; any other caller,
// direct or indirect, and a second installation are refused.
func SetReasonCheck(check func(code string, writing bool) error) error {
	pc, _, _, ok := runtime.Caller(1)
	fn := runtime.FuncForPC(pc)
	if !ok || fn == nil || !ownsReasonCheck(fn.Name()) {
		return errors.New("only the vocabulary package installs the reason check")
	}
	if check == nil || !reasonCheck.CompareAndSwap(nil, &check) {
		return errors.New("the reason check is installed once")
	}
	return nil
}

// ownsReasonCheck reports whether the runtime function name belongs to
// exactly the reason check's owner package (never a package elsewhere with
// the same name or suffix).
func ownsReasonCheck(fn string) bool { return funcPackage(fn) == reasonCheckOwner }

// funcPackage is the import path of a runtime function name.
func funcPackage(name string) string {
	slash := strings.LastIndexByte(name, '/')
	if dot := strings.IndexByte(name[slash+1:], '.'); dot >= 0 {
		return name[:slash+1+dot]
	}
	return name
}

func checkReason(code string, writing bool) error {
	check := reasonCheck.Load()
	if check == nil {
		return errors.New("no reason vocabulary is loaded (import internal/vocab)")
	}
	return (*check)(code, writing)
}

// base32 is the TypeID alphabet (Crockford, lowercase).
const base32 = "0123456789abcdefghjkmnpqrstvwxyz"

// CheckTypeID is the adc-typeid format: it decodes the 26-char suffix after
// the last underscore to 128 bits and requires a UUIDv7:
// version nibble 7 and the RFC 9562 variant (binary 10).
func CheckTypeID(s string) error {
	i := strings.LastIndexByte(s, '_')
	suffix := s[i+1:]
	if i < 0 || len(suffix) != 26 || suffix[0] > '7' {
		return errors.New("not a TypeID suffix")
	}
	var hi, lo uint64 // 130 bits read, the top 2 of the first char are zero
	for _, c := range []byte(suffix) {
		d := strings.IndexByte(base32, c)
		if d < 0 {
			return fmt.Errorf("invalid TypeID character %q", c)
		}
		hi = hi<<5 | lo>>59
		lo = lo<<5 | uint64(d)
	}
	if version := hi >> 12 & 0xf; version != 7 {
		return fmt.Errorf("TypeID is a UUIDv%d, want UUIDv7", version)
	}
	if variant := lo >> 62; variant != 2 {
		return errors.New("TypeID has a non-RFC 9562 UUID variant")
	}
	return nil
}

// integer is the only number spelling accepted: no fraction, no exponent, no
// leading zeros, no -0.
var integer = regexp.MustCompile(`^(0|-?[1-9][0-9]*)$`)

// Validate checks data against the named schema (e.g. "common/defs") as a
// document being read; Encode checks its output as one being written.
func Validate(schema string, data []byte) error {
	r, err := load(false)
	if err != nil {
		return err
	}
	return r.validate(schema, data)
}

func (r registry) validate(schema string, data []byte) error {
	s, ok := r[schema]
	if !ok {
		return &Error{Schema: schema, Detail: "unknown schema"}
	}
	if !utf8.Valid(data) {
		return &Error{Schema: schema, Detail: "not JSON: invalid UTF-8"}
	}
	if err := checkEscapes(data); err != nil {
		return &Error{Schema: schema, Detail: err.Error()}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	doc, err := readValue(dec)
	if err != nil {
		var se *strictError
		if errors.As(err, &se) {
			return &Error{Schema: schema, Detail: se.msg}
		}
		return &Error{Schema: schema, Detail: "not JSON: " + err.Error()}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return &Error{Schema: schema, Detail: "trailing data after the document"}
	}
	if err := s.Validate(doc); err != nil {
		return &Error{Schema: schema, Detail: err.Error()}
	}
	return nil
}

// checkEscapes walks the \u escapes inside string literals and refuses an
// unpaired surrogate, which encoding/json would turn into U+FFFD. Malformed
// JSON is left to the decoder.
func checkEscapes(data []byte) error {
	inString := false
	for i := 0; i < len(data); i++ {
		switch c := data[i]; {
		case c == '"':
			inString = !inString
		case c == '\\' && inString:
			i++
			if i >= len(data) || data[i] != 'u' {
				continue
			}
			r, ok := hex4(data, i+1)
			i += 4
			if !ok || !utf16.IsSurrogate(r) {
				continue
			}
			if r >= 0xdc00 { // a low surrogate with no high one before it
				return errors.New(`unpaired surrogate escape \u` + fmt.Sprintf("%04x", r))
			}
			if i+2 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return errors.New(`unpaired surrogate escape \u` + fmt.Sprintf("%04x", r))
			}
			if lo, ok := hex4(data, i+3); !ok || lo < 0xdc00 || lo > 0xdfff {
				return errors.New(`unpaired surrogate escape \u` + fmt.Sprintf("%04x", r))
			}
			i += 6
		}
	}
	return nil
}

func hex4(data []byte, at int) (rune, bool) {
	if at+4 > len(data) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(data[at:at+4]), 16, 32)
	return rune(n), err == nil
}

// strictError is a well-formed JSON document that breaks the stricter rules.
type strictError struct{ msg string }

func (e *strictError) Error() string { return e.msg }

// readValue builds the generic value from the token stream, refusing duplicate
// object keys at any depth and numbers not spelled as integers. The decoder
// bounds nesting (10000 levels), which bounds the recursion.
func readValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	switch t := tok.(type) {
	case json.Number:
		if !integer.MatchString(string(t)) {
			return nil, &strictError{"number " + string(t) + " is not spelled as an integer"}
		}
		return t, nil
	case json.Delim:
		switch t {
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := readValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			_, err := dec.Token() // ']'
			return arr, err
		case '{':
			obj := map[string]any{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string) // the decoder guarantees a string key here
				if _, dup := obj[k]; dup {
					return nil, &strictError{fmt.Sprintf("duplicate object key %q", k)}
				}
				v, err := readValue(dec)
				if err != nil {
					return nil, err
				}
				obj[k] = v
			}
			_, err := dec.Token() // '}'
			return obj, err
		}
		return nil, fmt.Errorf("unexpected %v", t)
	default: // string, bool, nil
		return t, nil
	}
}

// UnmarshalStrict decodes one JSON value into v, refusing unknown fields and
// trailing data. Custom UnmarshalJSON methods on contract types use it so
// that strictness does not stop at their boundary.
func UnmarshalStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the value")
	}
	return nil
}

// Decode validates data against the schema, then decodes the same bytes into
// a T, refusing unknown fields at every level the default decoder reaches.
func Decode[T any](schema string, data []byte) (T, error) {
	var out T
	r, err := load(false)
	if err != nil {
		return out, err
	}
	return decode[T](r, schema, data)
}

func decode[T any](r registry, schema string, data []byte) (T, error) {
	var out T
	if err := r.validate(schema, data); err != nil {
		return out, err
	}
	if err := UnmarshalStrict(data, &out); err != nil {
		return out, &Error{Schema: schema, Detail: "valid document does not fit the Go type: " + err.Error()}
	}
	return out, nil
}

// Encode marshals v and validates the result, so nothing reaches the wire
// unvalidated. The bytes are not canonical; digest the JCS form instead.
func Encode(schema string, v any) ([]byte, error) {
	r, err := load(true)
	if err != nil {
		return nil, err
	}
	return r.encode(schema, v)
}

func (r registry) encode(schema string, v any) ([]byte, error) {
	// v1 semantics, except that a string that is not valid UTF-8 is an error
	// rather than U+FFFD. The encoder checks every string it writes, so the
	// output of MarshalText and MarshalJSON methods too, and follows the
	// encoding/json field rules (embedding, "-", omitempty) by construction.
	data, err := jsonv2.Marshal(v,
		json.DefaultOptionsV1(),
		jsontext.AllowInvalidUTF8(false),
		jsontext.EscapeForHTML(false)) // <, > and & as themselves, as JSON.stringify writes them
	if err != nil {
		return nil, &Error{Schema: schema, Detail: "marshal: " + err.Error()}
	}
	if err := r.validate(schema, data); err != nil {
		return nil, err
	}
	return data, nil
}
