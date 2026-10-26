// SPDX-License-Identifier: Apache-2.0

// Package secretscan is the installed cycle harness's one search (#226
// entry 206): the forms a secret or a canary may take when it leaks, and a
// walk of directories that finds a file holding one, or an entry of a
// given name. The shell drivers call it through adc dev scan; no shell
// traversal decides what was searched.
package secretscan

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MinForm is the shortest form a search takes: a shorter one would match
// unrelated bytes (an empty one, everything). A secret with a form shorter
// than this is refused, as the shell's known refused it.
const MinForm = 16

// ErrShort: a secret or canary with a form shorter than MinForm.
var ErrShort = errors.New("a form shorter than 16 bytes")

var base64Text = regexp.MustCompile(`^[A-Za-z0-9+/]*={0,2}$`)

// SecretForms returns the forms a leak of secret (base64url, or a token
// tok_ID.SECRET) may take: as is; its secret part (past the first '.');
// that in standard base64; the secret part's first and last 16 bytes; the
// standard base64 of the whole; and, when the secret part is base64 by its
// text (a registry credential need not be), the hex of its bytes (Opus #443
// r2 B2: decided by the text, never by a producer's status).
func SecretForms(secret string) ([]string, error) {
	s := secret
	if i := strings.IndexByte(secret, '.'); i >= 0 {
		s = secret[i+1:]
	}
	std := strings.NewReplacer("-", "+", "_", "/").Replace(s)
	pad := std
	for len(pad)%4 != 0 {
		pad += "="
	}
	forms := []string{secret, s, std, s[:min(len(s), MinForm)], s[max(0, len(s)-MinForm):], base64.StdEncoding.EncodeToString([]byte(secret))}
	if base64Text.MatchString(pad) {
		b, err := base64.StdEncoding.DecodeString(pad)
		if err != nil {
			return nil, fmt.Errorf("secretscan: a base64 secret that does not decode: %w", err)
		}
		if len(b) > 0 {
			forms = append(forms, hex.EncodeToString(b))
		}
	}
	return checked(forms)
}

// CanaryForms returns the forms a canary line may take once it is carried
// by a request (cycle-expect.mjs's rule, ported once; Astra #440 H4): the
// line and its trimmed core, each as is, JSON-escaped (JavaScript's
// JSON.stringify) once and twice, Go-escaped (its encoding/json also
// escapes <, >, &, U+2028 and U+2029) once and twice, and the compositions
// of the two (JavaScript's inside Go's, Go's inside JavaScript's).
func CanaryForms(canary string) ([]string, error) {
	var forms []string
	for _, s := range []string{canary, jsTrim(canary)} {
		forms = append(forms, s, jsEscape(s), jsEscape(jsEscape(s)), goEscape(s), goEscape(goEscape(s)),
			goHTML(jsEscape(jsEscape(s))), jsEscape(goEscape(s)))
	}
	return checked(forms)
}

// checked: the forms without repeats, each at least MinForm bytes.
func checked(forms []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, f := range forms {
		if len(f) < MinForm {
			return nil, ErrShort
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out, nil
}

// jsEscape is JSON.stringify(s) without its quotes: ", \ and the control
// characters escaped (\b \f \n \r \t by name, the others \u00xx), all else
// as is.
func jsEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// goHTML replaces <, >, &, U+2028 and U+2029 by their \u escapes, as Go's
// encoding/json does on top of JSON's own escaping.
func goHTML(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '<', '>', '&', '\u2028', '\u2029':
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// jsTrim is JavaScript's String.prototype.trim: its WhiteSpace (tab, VT,
// FF, space, U+00A0, U+FEFF and every Zs) and its LineTerminators (LF, CR,
// U+2028, U+2029), from both ends; never U+0085, which Go's TrimSpace
// takes, nor keeping U+FEFF, which it keeps (Opus #443 r3 O3).
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case '\t', '\v', '\f', ' ', '\u00a0', '\ufeff', '\n', '\r', '\u2028', '\u2029':
			return true
		}
		return unicode.Is(unicode.Zs, r)
	})
}

// goEscape is cycle-expect.mjs's goEsc: JSON's escaping, then Go's HTML
// escaping.
func goEscape(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	return goHTML(jsEscape(s))
}
