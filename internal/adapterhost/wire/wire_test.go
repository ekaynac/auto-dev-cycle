// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
)

// wireCase is one case of fixtures/contract/wire-cases.json; the TypeScript
// side (adapters/contract/test/wire-cases.test.ts) runs the same file.
type wireCase struct {
	Name  string   `json:"name"`
	Text  *string  `json:"text"`
	Hex   *string  `json:"hex"`
	Nest  int      `json:"nest"`
	PadTo int      `json:"pad_to"`
	Want  []string `json:"want"`
}

func (c wireCase) bytes(t *testing.T) []byte {
	switch {
	case c.Hex != nil:
		b, err := hex.DecodeString(*c.Hex)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		return b
	case c.Nest > 0:
		return []byte(strings.Repeat("[", c.Nest) + strings.Repeat("]", c.Nest) + "\n")
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

// verdicts reads a stream as the host does, one verdict per line.
func verdicts(r io.Reader) []string {
	var got []string
	rd := NewReader(r)
	for {
		line, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			if !errors.Is(err, ErrFraming) {
				return append(got, "error: "+err.Error())
			}
			return append(got, "framing")
		}
		m, err := Classify(line)
		var ref *Refusal
		switch {
		case errors.As(err, &ref):
			got = append(got, ref.Stage)
		case err != nil:
			got = append(got, "error: "+err.Error())
		case m.Kind == KindEvent:
			got = append(got, "event")
		case m.Kind == KindResponse:
			got = append(got, "response")
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
		// Whole, and one byte per read, so line boundaries fall anywhere.
		for _, r := range []io.Reader{bytes.NewReader(in), iotest.OneByteReader(bytes.NewReader(in))} {
			if c.PadTo > 0 && len(in) > 1<<20 {
				if _, ok := r.(*bytes.Reader); !ok {
					continue // too slow byte by byte; chunking is covered below
				}
			}
			got := verdicts(r)
			if got == nil {
				got = []string{}
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s: got %v, want %v", c.Name, got, want)
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
