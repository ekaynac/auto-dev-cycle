// SPDX-License-Identifier: Apache-2.0

// Package wire is the adapter protocol's framing and envelope on the core's
// side (docs/design/m0/09 "Process model"): newline-delimited JSON-RPC 2.0
// over stdio, one message per line, UTF-8, at most 16 MiB. It is meant to
// give the same verdict on every byte stream as the TypeScript SDK
// (adapters/contract framing.ts, strict-json.ts, envelope.ts):
// fixtures/contract/wire-cases.json is the corpus both run, comparing
// verdicts and decoded values; it is evidence of parity, not a proof.
package wire

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// MaxMessageBytes bounds one message, its newline not counted.
const MaxMessageBytes = 16 << 20

// readChunk is the reader's buffer: a line longer than this is gathered in
// pieces, and gathering stops as soon as the bound is passed.
const readChunk = 64 << 10

// ErrFraming is wrapped by every framing error. A framing error ends the
// stream: the reader is not usable after it.
var ErrFraming = errors.New("adapter framing")

func framingError(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrFraming}, args...)...)
}

// Reader yields one message per line.
type Reader struct {
	br  *bufio.Reader
	max int
	err error
}

// NewReader reads messages from r, each at most MaxMessageBytes.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, readChunk), max: MaxMessageBytes}
}

// Next returns the next line without its newline. It returns io.EOF at a
// clean end of input, and an error wrapping ErrFraming for a line over the
// bound, a line that is not valid UTF-8, or input that ends inside a line
// (a truncated message). Its storage is proportional to the message bound:
// a line is refused once it passes the bound, but append growth may reserve
// more than the bound, and briefly hold the old and new arrays together.
// Decoding a line (Classify) allocates further, in proportion to its
// structure; no tighter budget is claimed or measured here.
func (r *Reader) Next() ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	line, err := r.next()
	if err != nil {
		r.err = err
		return nil, err
	}
	return line, nil
}

func (r *Reader) next() ([]byte, error) {
	var line []byte
	for {
		piece, err := r.br.ReadSlice('\n')
		body := bytes.TrimSuffix(piece, []byte{'\n'})
		if len(line)+len(body) > r.max {
			return nil, framingError("a message exceeds %d bytes", r.max)
		}
		line = append(line, body...)
		switch {
		case err == nil:
			if !utf8.Valid(line) {
				return nil, framingError("a message is not valid UTF-8")
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) == 0:
			return nil, io.EOF
		case errors.Is(err, io.EOF):
			return nil, framingError("the input ended inside a message")
		default:
			return nil, err
		}
	}
}

// Writer writes one message per line.
type Writer struct {
	w   io.Writer
	max int
}

// NewWriter writes messages to w, each at most MaxMessageBytes.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w, max: MaxMessageBytes} }

// Write sends one encoded message and its newline in a single write. A
// message over the bound, with a newline in it, or not valid UTF-8 is
// refused with an error wrapping ErrFraming, and nothing is written.
func (w *Writer) Write(msg []byte) error {
	switch {
	case len(msg) > w.max:
		return framingError("a message exceeds %d bytes", w.max)
	case bytes.IndexByte(msg, '\n') >= 0:
		return framingError("a message contains a newline")
	case !utf8.Valid(msg):
		return framingError("a message is not valid UTF-8")
	}
	line := make([]byte, 0, len(msg)+1)
	_, err := w.w.Write(append(append(line, msg...), '\n'))
	return err
}
