// SPDX-License-Identifier: Apache-2.0

package cli

// Output of what the controller sends. Every string in it is untrusted (a
// note's body, 08 "Notes", but also ids, reasons and details): human
// output is written only through say, which escapes every argument so
// nothing can move, hide or restyle other text in a terminal (issue #156
// (c)). --json output is written only through sayJSON.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ekaynac/auto-dev-cycle/internal/client"
)

// safe escapes, visibly, every rune that is not plainly visible text:
// controls (C0, DEL, C1: terminal escapes among them), format characters
// (bidi overrides and isolates, zero-width ones: Cf is not graphic), line
// and paragraph separators, other default-ignorable ones, invalid UTF-8,
// and the backslash, so no escape can be forged.
func safe(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\\':
			b.WriteString(`\\`)
		case !unicode.IsGraphic(r) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r):
			fmt.Fprintf(&b, `\u{%04X}`, r)
		default:
			b.WriteRune(r)
		}
		i += n
	}
	return b.String()
}

// say is the one writer of human output: format is the CLI's own (its
// verbs %s), and every argument is escaped by safe.
func say(w io.Writer, format string, a ...any) error {
	esc := make([]any, len(a))
	for i, v := range a {
		esc[i] = safe(fmt.Sprint(v))
	}
	_, err := fmt.Fprintf(w, format, esc...)
	return err
}

// sayJSON is the one writer of --json output: one validated JSON document
// on a line, as sent, except that a DEL or C1 control (legal unescaped in
// a JSON string, and a terminal control) or an invalid UTF-8 byte is
// written as its \u escape, which leaves the document's value unchanged.
// JSON itself admits no raw C0 control in a string.
func sayJSON(w io.Writer, raw []byte) error {
	raw = bytes.TrimSpace(raw)
	var b bytes.Buffer
	for i := 0; i < len(raw); {
		r, n := utf8.DecodeRune(raw[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			b.WriteString(`\ufffd`)
		case r >= 0x7f && r <= 0x9f:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.Write(raw[i : i+n])
		}
		i += n
	}
	b.WriteByte('\n')
	_, err := w.Write(b.Bytes())
	return err
}

// render writes a validated read: each record under its resource and id,
// then its other fields in name order.
func render(w io.Writer, resource string, raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return err
	}
	items := []any{doc}
	if resource != client.Whoami {
		items, _ = doc["items"].([]any)
	}
	if len(items) == 0 {
		return say(w, "%s: none readable\n", resource)
	}
	var b strings.Builder
	for _, it := range items {
		rec, _ := it.(map[string]any)
		id := rec["id"]
		if resource == client.Whoami {
			id = rec["actor"]
		}
		say(&b, "%s %s\n", resource, id)
		keys := make([]string, 0, len(rec))
		for k := range rec {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			field(&b, k, rec[k])
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// field writes one field: a multi-line string as a block, each line
// marked, so it cannot pass for another field or record.
func field(b *strings.Builder, k string, v any) {
	s, isString := v.(string)
	switch {
	case isString && strings.Contains(s, "\n"):
		say(b, "  %s:\n", k)
		for _, line := range strings.Split(s, "\n") {
			say(b, "    | %s\n", line)
		}
		return
	case v == nil:
		s = "-"
	case !isString:
		j, _ := json.Marshal(v)
		s = string(j)
	}
	say(b, "  %s: %s\n", k, s)
}
