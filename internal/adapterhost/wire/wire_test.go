// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

// wireCase is one case of fixtures/contract/wire-cases.json; the TypeScript
// side (adapters/contract/test/wire-cases.test.ts) runs the same file.
type wireCase struct {
	Name    string            `json:"name"`
	Text    *string           `json:"text"`
	Hex     *string           `json:"hex"`
	Nest    *nestSpec         `json:"nest"`
	PadTo   int               `json:"pad_to"`
	Want    []string          `json:"want"`
	Decoded []json.RawMessage `json:"decoded"`
}

// nestSpec builds prefix, open×times, inner, close×times, suffix, newline.
type nestSpec struct {
	Prefix, Open, Inner, Close, Suffix string
	Times                              int
}

func (c wireCase) bytes(t *testing.T) []byte {
	switch {
	case c.Hex != nil:
		b, err := hex.DecodeString(*c.Hex)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		return b
	case c.Nest != nil:
		n := c.Nest
		return []byte(n.Prefix + strings.Repeat(n.Open, n.Times) + n.Inner + strings.Repeat(n.Close, n.Times) + n.Suffix + "\n")
	case c.PadTo > 0:
		first, rest, _ := strings.Cut(*c.Text, "\n")
		if !strings.HasPrefix(first, "{") || len(first) > c.PadTo {
			t.Fatalf("%s: cannot pad", c.Name)
		}
		return []byte("{" + strings.Repeat(" ", c.PadTo-len(first)) + first[1:] + "\n" + rest)
	case c.Text != nil:
		return []byte(*c.Text)
	}
	t.Fatalf("%s: no bytes", c.Name)
	return nil
}

// generic decodes JSON the ordinary way, numbers as json.Number.
func generic(t *testing.T, b []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decoding %.60s: %v", b, err)
	}
	return v
}

// read reads a stream as the host does: one verdict per line, the strict
// value of each line the strict reader accepts, and the classified contents
// checked against that value.
func read(t *testing.T, r io.Reader) (verdicts []string, values []any, lines [][]byte) {
	rd := NewReader(r)
	for {
		line, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return verdicts, values, lines
		}
		if err != nil {
			if !errors.Is(err, ErrFraming) {
				return append(verdicts, "error: "+err.Error()), values, lines
			}
			if _, again := rd.Next(); !errors.Is(again, ErrFraming) {
				t.Errorf("the reader read on after a framing error: %v", again)
			}
			return append(verdicts, "framing"), values, lines
		}
		if v, err := contract.ReadStrict(line); err == nil {
			values, lines = append(values, v), append(lines, line)
		}
		m, err := Classify(line)
		var ref *Refusal
		switch {
		case errors.As(err, &ref):
			verdicts = append(verdicts, ref.Stage)
		case err != nil:
			verdicts = append(verdicts, "error: "+err.Error())
		case m.Kind == KindEvent:
			verdicts = append(verdicts, "event")
			if got, want := generic(t, m.Event), values[len(values)-1].(map[string]any)["params"]; !reflect.DeepEqual(got, want) {
				t.Errorf("event params %v, want %v", got, want)
			}
		case m.Kind == KindResponse:
			verdicts = append(verdicts, "response")
			checkResponse(t, m.Response, values[len(values)-1].(map[string]any))
		}
	}
}

// checkResponse compares a classified response with the strict value.
func checkResponse(t *testing.T, r *Response, v map[string]any) {
	t.Helper()
	if id := v["id"].(json.Number).String(); strconv.FormatInt(r.ID, 10) != id {
		t.Errorf("id %d, want %s", r.ID, id)
	}
	if result, ok := v["result"]; ok && !reflect.DeepEqual(generic(t, r.Result), result) {
		t.Errorf("result %s, want %v", r.Result, result)
	}
	if e, ok := v["error"].(map[string]any); ok {
		data, hasData := e["data"]
		if r.Error == nil || strconv.FormatInt(r.Error.Code, 10) != e["code"].(json.Number).String() || r.Error.Message != e["message"] ||
			hasData != (r.Error.Data != nil) || hasData && !reflect.DeepEqual(generic(t, r.Error.Data), data) {
			t.Errorf("error %+v, want %v", r.Error, e)
		}
	}
}

func TestSharedWireCases(t *testing.T) {
	b, err := os.ReadFile("../../../fixtures/contract/wire-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct{ Cases []wireCase }
	if err := json.Unmarshal(b, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("no wire cases")
	}
	for _, c := range corpus.Cases {
		in := c.bytes(t)
		want := c.Want
		if want == nil {
			want = []string{}
		}
		// Whole, and one byte per read (except the 16 MiB cases), so line
		// boundaries and multibyte sequences fall across reads.
		readers := []io.Reader{bytes.NewReader(in)}
		if len(in) <= 1<<20 {
			readers = append(readers, iotest.OneByteReader(bytes.NewReader(in)))
		}
		for _, r := range readers {
			got, values, lines := read(t, r)
			if got == nil {
				got = []string{}
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s: got %v, want %v", c.Name, got, want)
			}
			if c.Nest != nil {
				// Generated: the ordinary decoder is the reference.
				for i, v := range values {
					if !reflect.DeepEqual(v, generic(t, lines[i])) {
						t.Errorf("%s: strict value differs from the ordinary decoder's", c.Name)
					}
				}
				continue
			}
			if len(values) != len(c.Decoded) {
				t.Errorf("%s: %d strict values, the corpus lists %d", c.Name, len(values), len(c.Decoded))
				continue
			}
			for i, v := range values {
				if want := generic(t, c.Decoded[i]); !reflect.DeepEqual(v, want) {
					t.Errorf("%s: line %d decoded %v, want %v", c.Name, i, v, want)
				}
			}
		}
	}
}

func TestReaderBoundsAcrossChunks(t *testing.T) {
	// A line just over the bound, delivered in reads smaller than the buffer.
	line := strings.Repeat("x", 3*readChunk+1)
	rd := NewReader(iotest.HalfReader(strings.NewReader(line + "\n")))
	rd.max = 3 * readChunk
	if _, err := rd.Next(); !errors.Is(err, ErrFraming) {
		t.Fatalf("over the bound: %v", err)
	}
	if _, err := rd.Next(); !errors.Is(err, ErrFraming) {
		t.Fatalf("after a framing error: %v", err)
	}
	rd = NewReader(strings.NewReader(line[1:] + "\n"))
	rd.max = 3 * readChunk
	if got, err := rd.Next(); err != nil || string(got) != line[1:] {
		t.Fatalf("at the bound: %d bytes, %v", len(got), err)
	}
}

func TestReaderPassesReadErrors(t *testing.T) {
	boom := errors.New("boom")
	if _, err := NewReader(iotest.ErrReader(boom)).Next(); !errors.Is(err, boom) || errors.Is(err, ErrFraming) {
		t.Fatalf("got %v", err)
	}
}

func TestWriter(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out)
	w.max = 10
	if err := w.Write([]byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"a":"xxxx"}`, "{\n}", "\"\xff\""} {
		if err := w.Write([]byte(bad)); !errors.Is(err, ErrFraming) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if got := out.String(); got != "{\"a\":1}\n" {
		t.Fatalf("wrote %q", got)
	}
	exact := bytes.Repeat([]byte("1"), 10)
	if err := w.Write(exact); err != nil {
		t.Fatalf("exactly the bound: %v", err)
	}
}

func TestEncodeAndClassifyRoundTrip(t *testing.T) {
	req, err := Encode(Request{JSONRPC: Version, ID: 7, Method: "describe"})
	if err != nil || string(req) != `{"jsonrpc":"2.0","id":7,"method":"describe"}` {
		t.Fatalf("request %s, %v", req, err)
	}
	if _, err := Encode(Request{JSONRPC: Version, Method: "x\xff"}); err == nil {
		t.Fatal("invalid UTF-8 encoded")
	}
	if b, _ := Encode(map[string]string{"a": "<&>"}); string(b) != `{"a":"<&>"}` {
		t.Fatalf("HTML escaped: %s", b)
	}
	m, err := Classify([]byte(`{"jsonrpc":"2.0","id":3,"error":{"code":-32602,"message":"bad","data":[1],"Message":2}}`))
	if err != nil || m.Kind != KindResponse || m.Response.ID != 3 || m.Response.Result != nil {
		t.Fatalf("%+v, %v", m, err)
	}
	if e := m.Response.Error; e.Code != -32602 || e.Message != "bad" || string(e.Data) != "[1]" {
		t.Fatalf("error %+v", e)
	}
	m, err = Classify([]byte(`{"jsonrpc":"2.0","id":4,"result":null}`))
	if err != nil || string(m.Response.Result) != "null" || m.Response.Error != nil {
		t.Fatalf("null result: %+v, %v", m.Response, err)
	}
	m, err = Classify([]byte(`{"params":{"event":"heartbeat"},"method":"adapter.event","jsonrpc":"2.0"}`))
	if err != nil || m.Kind != KindEvent || string(m.Event) != `{"event":"heartbeat"}` {
		t.Fatalf("event: %+v, %v", m, err)
	}
}
