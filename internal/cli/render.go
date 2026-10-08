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
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

// safe escapes, visibly, every rune that is not plainly visible text:
// controls (C0, DEL, C1: terminal escapes among them), format characters
// (bidi overrides and isolates, zero-width ones: Cf is not graphic), line
// and paragraph separators, other default-ignorable ones and variation
// selectors, the blank braille pattern, a combining mark that would join
// the text before the value, invalid UTF-8, and the backslash, so no
// escape can be forged.
func safe(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\\':
			b.WriteString(`\\`)
		case !unicode.IsGraphic(r) || unicode.In(r, unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector) ||
			r == brailleBlank || (i == 0 && unicode.In(r, unicode.Mn, unicode.Me)):
			fmt.Fprintf(&b, `\u{%04X}`, r)
		default:
			b.WriteRune(r)
		}
		i += n
	}
	return b.String()
}

// brailleBlank is graphic, yet shows nothing.
const brailleBlank = '\u2800'

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
// on one line, compacted (no CR, LF or tab between its tokens, and JSON
// admits no raw C0 control inside a string), with every DEL or C1 control
// (legal unescaped in a JSON string, and a terminal control), line or
// paragraph separator (a line break to some NDJSON readers) or invalid
// UTF-8 byte written as its \u escape: the document's value is unchanged.
func sayJSON(w io.Writer, doc []byte) error {
	var c bytes.Buffer
	if err := json.Compact(&c, doc); err != nil {
		return errors.New("the controller's document is not JSON")
	}
	raw := c.Bytes()
	var b bytes.Buffer
	for i := 0; i < len(raw); {
		r, n := utf8.DecodeRune(raw[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			b.WriteString(`\ufffd`)
		case (r >= 0x7f && r <= 0x9f) || r == '\u2028' || r == '\u2029':
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

// Say, SayJSON and Parse are say, sayJSON and parse for the other command
// lines (adc-harness): one escaping for all human and --json output.
func Say(w io.Writer, format string, a ...any) error { return say(w, format, a...) }

func SayJSON(w io.Writer, doc []byte) error { return sayJSON(w, doc) }

func Parse(fs *flag.FlagSet, args []string, stderr io.Writer) (code int, ok bool) {
	return parse(fs, args, stderr)
}

// parse parses fs's flags from args with nothing printed by the flag
// package: a usage error goes through say, and -h prints the CLI's own
// defaults. ok is false when the command ends with code.
func parse(fs *flag.FlagSet, args []string, stderr io.Writer) (code int, ok bool) {
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fs.SetOutput(stderr)
		fs.PrintDefaults()
		return entry.ExitOK, false
	case err != nil:
		say(stderr, "%s: %s\n", fs.Name(), err)
		return entry.ExitUsage, false
	}
	return 0, true
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
