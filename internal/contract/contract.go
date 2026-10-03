// SPDX-License-Identifier: Apache-2.0

// Package contract validates, decodes and encodes contract documents against
// the embedded schemas (docs/design/m0/01 "Code generation", decoding and
// encoding contract):
//   - Decode validates the bytes, then decodes the same bytes strictly;
//   - trailing data after the document is refused;
//   - numbers must be spelled as integers (M0 contracts have no floats), and -0
//     is refused, so Go and TypeScript agree on identity;
//   - duplicate object keys (at any depth) and invalid UTF-8 are refused:
//     encoding/json would silently keep the last duplicate or substitute
//     U+FFFD, while JCS (event hashes, input digests) refuses them;
//   - Encode validates its own output before returning it. Its bytes are not a
//     canonical form (key order follows the Go struct): every digest is taken
//     over the JCS canonicalisation, never over Encode output.
package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ekaynac/auto-dev-cycle/schemas"
)

// Error reports a document that does not satisfy its contract.
type Error struct {
	Schema string // schema path, e.g. "records/receipt"
	Detail string
}

func (e *Error) Error() string { return "contract " + e.Schema + ": " + e.Detail }

var (
	loadOnce sync.Once
	compiled map[string]*jsonschema.Schema
	loadErr  error
)

// load compiles every embedded schema once. A schema that does not compile is
// a build defect, reported on every call.
func load() (map[string]*jsonschema.Schema, error) {
	loadOnce.Do(func() {
		paths, err := schemas.Paths()
		if err != nil {
			loadErr = err
			return
		}
		c := jsonschema.NewCompiler()
		for _, p := range paths {
			b, err := fs.ReadFile(schemas.FS(), p)
			if err != nil {
				loadErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
			if err != nil {
				loadErr = fmt.Errorf("schema %s: %w", p, err)
				return
			}
			if err := c.AddResource(schemas.Base+p, doc); err != nil {
				loadErr = fmt.Errorf("schema %s: %w", p, err)
				return
			}
		}
		out := map[string]*jsonschema.Schema{}
		for _, p := range paths {
			s, err := c.Compile(schemas.Base + p)
			if err != nil {
				loadErr = fmt.Errorf("schema %s: %w", p, err)
				return
			}
			out[strings.TrimSuffix(p, ".json")] = s
		}
		compiled = out
	})
	return compiled, loadErr
}

// integer is the only number spelling accepted: no fraction, no exponent, no
// leading zeros, no -0.
var integer = regexp.MustCompile(`^(0|-?[1-9][0-9]*)$`)

// Validate checks data against the named schema (e.g. "records/receipt").
func Validate(schema string, data []byte) error {
	all, err := load()
	if err != nil {
		return err
	}
	s, ok := all[schema]
	if !ok {
		return &Error{Schema: schema, Detail: "unknown schema"}
	}
	if !utf8.Valid(data) {
		return &Error{Schema: schema, Detail: "not JSON: invalid UTF-8"}
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

// Decode validates data against the schema, then decodes the same bytes into
// a T, refusing unknown fields at every level.
func Decode[T any](schema string, data []byte) (T, error) {
	var out T
	if err := Validate(schema, data); err != nil {
		return out, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, &Error{Schema: schema, Detail: "valid document does not fit the Go type: " + err.Error()}
	}
	return out, nil
}

// Encode marshals v and validates the result, so nothing reaches the wire
// unvalidated. The bytes are not canonical; digest the JCS form instead.
func Encode(schema string, v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // <, > and & as themselves, as JSON.stringify writes them
	if err := enc.Encode(v); err != nil {
		return nil, &Error{Schema: schema, Detail: "marshal: " + err.Error()}
	}
	data := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if err := Validate(schema, data); err != nil {
		return nil, err
	}
	return data, nil
}
