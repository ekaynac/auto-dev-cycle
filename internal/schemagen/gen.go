// SPDX-License-Identifier: Apache-2.0

// Package schemagen generates Go types from the schema files, for exactly the
// grammar in docs/design/m0/01 "Code generation" (spike report:
// docs/research/2026-10-03-codegen-spike/). Any other shape is refused with
// its location, so nothing degrades to interface{}. Each shape has a keyword
// allow-list (see shapeKeys); a keyword outside it is refused rather than
// ignored, because an ignored keyword means the Go type and the schema
// disagree. Every emitted package-level identifier and every struct field
// name is checked for collisions. The types are an ergonomic view:
// internal/contract validates every document against the schema before
// decoding and after encoding.
package schemagen

import (
	"bytes"
	"fmt"
	"go/format"
	"go/token"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const maxSafeInt = 9007199254740991 // 2^53-1: integers stay exact in JavaScript

// VocabPackage is the import path of the only package that may install the
// reason check in this module's generated code.
const VocabPackage = "github.com/ekaynac/auto-dev-cycle/internal/vocab"

// ReasonTypeName is the one name a reason code declaration may have, so the
// generated unchecked constructor is always ReasonCodeOf (which the
// vocabulary check guards).
const ReasonTypeName = "ReasonCode"

// Generate returns one Go source file per schema file (keyed by output file
// name) in package pkg, for this module (GenerateFor with VocabPackage).
// files maps schema paths ("records/receipt.json") to their bytes.
func Generate(files map[string][]byte, pkg string) (map[string][]byte, error) {
	return GenerateFor(files, pkg, VocabPackage)
}

// GenerateFor is Generate for a module whose vocabulary package (the only
// installer of the reason check) has the import path vocabPkg.
func GenerateFor(files map[string][]byte, pkg, vocabPkg string) (map[string][]byte, error) {
	g := &gen{
		roots: map[string]*Node{}, names: map[string]string{}, taken: map[string]string{},
		envelopes: map[string]bool{}, isRoot: map[*Node]bool{},
		emitted: map[string]string{"decodeStrict": "helpers.gen.go", "SetReasonCheck": "helpers.gen.go"},
	}
	paths := make([]string, 0, len(files))
	for p, b := range files {
		n, err := Parse(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if n.Kind != Object {
			return nil, fmt.Errorf("%s: a schema file must be an object", p)
		}
		g.roots[p], g.isRoot[n] = n, true
		paths = append(paths, p)
	}
	sort.Strings(paths)
	// A reason code is checked only through its opaque type, so the format
	// may appear in exactly one place: the $defs/ReasonCode string
	// declaration. Anywhere else (inline, an enum, a map key, an item, a
	// refinement) it would generate an unchecked string.
	for _, p := range paths {
		if err := reasonFormatOnlyDeclared(g.roots[p], ""); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	// Pass 1: name every top-level type and $defs entry, refusing collisions.
	for _, p := range paths {
		root := g.roots[p]
		if root.Get("type") == nil && root.Get("oneOf") == nil {
			for _, k := range root.Keys {
				if !annotations[k] && !rootKeys[k] {
					return nil, fmt.Errorf("%s: keyword %q at the root of a file without a root type is not supported", p, k)
				}
			}
		} else {
			title := root.Get("title")
			if title == nil {
				return nil, fmt.Errorf("%s: a root type needs a title", p)
			}
			if err := g.name(p+"#", title.Str); err != nil {
				return nil, err
			}
		}
		if defs := root.Get("$defs"); defs != nil {
			if defs.Kind != Object {
				return nil, fmt.Errorf("%s: $defs must be an object", p)
			}
			for _, k := range defs.Keys {
				if err := g.name(p+"#/$defs/"+k, k); err != nil {
					return nil, err
				}
				if isEnvelope(defs.Get(k)) {
					g.envelopes[k] = true
				}
			}
		}
	}
	// Pass 2: emit.
	out := map[string][]byte{}
	outFrom := map[string]string{helpersFile: "the generated helpers"}
	for _, p := range paths {
		name := strings.ReplaceAll(strings.TrimSuffix(p, ".json"), "/", "_") + ".gen.go"
		if prev, clash := outFrom[name]; clash {
			return nil, fmt.Errorf("%s: output file %s is also the one for %s", p, name, prev)
		}
		outFrom[name] = p
		g.file, g.base, g.buf, g.needs = p, p, &bytes.Buffer{}, map[string]bool{}
		root := g.roots[p]
		if name, ok := g.names[p+"#"]; ok {
			g.declare(name, root)
		}
		if defs := root.Get("$defs"); defs != nil {
			for _, k := range defs.Keys {
				g.declare(k, defs.Get(k))
			}
		}
		if g.err != nil {
			return nil, g.err
		}
		src, err := g.finish(pkg)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out[name] = src
	}
	helpers, err := format.Source([]byte(fmt.Sprintf(helpersSrc, pkg, vocabPkg)))
	if err != nil {
		return nil, err
	}
	out[helpersFile] = helpers
	return out, nil
}

// helpersFile holds decodeStrict; no schema file may generate it.
const helpersFile = "helpers.gen.go"

type gen struct {
	roots  map[string]*Node
	isRoot map[*Node]bool    // file roots: may also carry $schema, $id and $defs
	names  map[string]string // ref key → Go name
	taken  map[string]string // Go name → ref key
	// emitted: every package-level identifier written so far → its location.
	// Nested type names (Parent+Field, +Item, +Value, +List, variants), enum
	// constants and Variant interfaces share the package with the $defs.
	emitted map[string]string
	// envelopes: open objects with properties, usable only as an allOf base
	envelopes map[string]bool
	file      string // the file being emitted; error locations name it
	base      string // the file the node being read comes from; $refs resolve against it
	buf       *bytes.Buffer
	needs     map[string]bool // imports
	err       error
}

func (g *gen) name(key, name string) error {
	if !token.IsIdentifier(name) || !token.IsExported(name) {
		return fmt.Errorf("%s: %q is not an exported Go identifier", key, name)
	}
	if prev, ok := g.taken[name]; ok {
		return fmt.Errorf("type name %s is used by both %s and %s", name, prev, key)
	}
	g.taken[name], g.names[key] = key, name
	return nil
}

func (g *gen) fail(where, format string, args ...any) string {
	if g.err == nil {
		g.err = fmt.Errorf("%s: %s: unsupported shape: %s", g.file, where, fmt.Sprintf(format, args...))
	}
	return "invalid"
}

// emit claims a package-level identifier; a second claim is a collision that
// would otherwise surface only when the generated package fails to compile.
func (g *gen) emit(ident, where string) bool {
	if !token.IsIdentifier(ident) || !token.IsExported(ident) {
		g.fail(where, "generated name %q is not an exported Go identifier", ident)
		return false
	}
	if prev, ok := g.emitted[ident]; ok {
		g.fail(where, "generated name %s collides with the one generated for %s", ident, prev)
		return false
	}
	g.emitted[ident] = g.file + ": " + where
	return true
}

// field returns the Go field name for JSON key k, refusing an empty or
// invalid name and a second field with the same name in seen.
func (g *gen) field(k, where string, seen map[string]string) string {
	if !validTag(k) {
		g.fail(where, "property %q cannot be written as a Go JSON tag name (encoding/json would change or drop it)", k)
		return "Invalid"
	}
	f := goName(k)
	if !token.IsIdentifier(f) || !token.IsExported(f) {
		g.fail(where, "property %q gives no exported Go field name (%q)", k, f)
		return "Invalid"
	}
	if prev, ok := seen[f]; ok {
		g.fail(where, "properties %q and %q both give the Go field name %s", prev, k, f)
		return "Invalid"
	}
	seen[f] = k
	return f
}

// validTag mirrors encoding/json's rule for tag names: letters, digits and
// a fixed set of punctuation. "-" alone means "skip" and is refused too.
func validTag(k string) bool {
	if k == "" || k == "-" {
		return false
	}
	for _, c := range k {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c):
		case unicode.IsLetter(c) || unicode.IsDigit(c):
		default:
			return false
		}
	}
	return true
}

// Keyword allow-lists. Annotations are accepted everywhere; rootKeys only at
// the root of a file. Validation-only keywords that leave the Go shape
// unchanged (string length and pattern, integer bounds, array length and
// uniqueness, map key patterns) are listed per shape; anything else is
// refused, e.g. patternProperties, dependentSchemas, anyOf, prefixItems and
// contains. Two validation-only forms change no Go shape and are accepted:
// a $ref to a string schema refined by string keywords, and if/then/else
// conditions that only constrain properties the object already declares.
var (
	annotations = keySet("description", "title", "$comment")
	rootKeys    = keySet("$schema", "$id", "$defs")
	shapeKeys   = map[string]map[string]bool{
		"$ref":         keySet("$ref"),
		"refinement":   keySet("$ref", "pattern", "minLength", "maxLength", "format"),
		"oneOf":        keySet("oneOf"),
		"enum":         keySet("enum", "type"),
		"const":        keySet("const", "type"),
		"null":         keySet("type"),
		"string":       keySet("type", "minLength", "maxLength", "pattern", "format"),
		"boolean":      keySet("type"),
		"integer":      keySet("type", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"),
		"array":        keySet("type", "items", "minItems", "maxItems", "uniqueItems"),
		"open payload": keySet("type", "x-adc-open-payload"),
		"map":          keySet("type", "additionalProperties", "propertyNames", "minProperties", "maxProperties"),
		"struct":       keySet("type", "properties", "required", "additionalProperties", "unevaluatedProperties", "allOf", "if", "then", "else"),
		"envelope":     keySet("type", "properties", "required"),
		"if/then":      keySet("if", "then"),
		"if":           keySet("properties", "required"),
		"then":         keySet("properties"),
		"conditional":  keySet("if", "then", "else"),
		"condition":    keySet("properties", "required"),
		"constraint":   keySet("const", "enum", "not", "minLength", "maxLength", "pattern", "minimum", "maximum"),
		"keyed branch": keySet("type", "additionalProperties", "required", "properties"),
		"key names":    keySet("type", "enum", "pattern", "minLength", "maxLength", "format"),
	}
)

func keySet(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// only refuses any keyword of n outside the allow-list of shape.
func (g *gen) only(n *Node, where, shape string) bool {
	if n == nil || n.Kind != Object {
		g.fail(where, "a %s schema must be an object (boolean schemas are not supported)", shape)
		return false
	}
	allowed := shapeKeys[shape]
	for _, k := range n.Keys {
		if allowed[k] || annotations[k] || (g.isRoot[n] && rootKeys[k]) {
			continue
		}
		g.fail(where, "keyword %q is not supported on a %s schema", k, shape)
		return false
	}
	return true
}

// deref follows a chain of $ref of any length to the schema it names (nil if
// dangling or cyclic), resolving each hop against the file it appears in.
func (g *gen) deref(n *Node) *Node {
	n, _ = g.derefFrom(g.base, n)
	return n
}

func (g *gen) derefFrom(base string, n *Node) (*Node, string) {
	seen := map[string]bool{}
	for n != nil {
		ref := n.Get("$ref")
		if ref == nil {
			return n, base
		}
		var key string
		n, base, key = g.resolveFrom(base, ref.Str)
		if seen[key] {
			return nil, "" // a $ref cycle names no schema
		}
		seen[key] = true
	}
	return nil, ""
}

// within reads nodes of another file: $refs inside them resolve against it.
func (g *gen) within(base string, f func()) {
	prev := g.base
	g.base = base
	f()
	g.base = prev
}

// isNullable: n, after following every $ref (aliases and other files
// included), is a oneOf [null, X].
func (g *gen) isNullable(n *Node, where string) bool {
	t := g.deref(n)
	if t == nil {
		g.fail(where, "the $ref chain does not resolve (dangling or cyclic)")
		return true
	}
	one := t.Get("oneOf")
	return one != nil && len(one.Items) == 2 && (isNull(one.Items[0]) || isNull(one.Items[1]))
}

// isCollection: an array or a typed map, after following $ref.
func (g *gen) isCollection(n *Node) bool {
	n = g.deref(n)
	t := n.Get("type")
	if t == nil {
		return false
	}
	ap := n.Get("additionalProperties")
	return t.Str == "array" || (t.Str == "object" && ap != nil && ap.Kind == Object && n.Get("properties") == nil)
}

func (g *gen) p(format string, args ...any) { fmt.Fprintf(g.buf, format+"\n", args...) }

func (g *gen) doc(n *Node, name string) {
	if d := n.Get("description"); d != nil {
		g.p("// %s: %s", name, strings.ReplaceAll(d.Str, "\n", "\n// "))
	}
}

// resolve follows a $ref written in the current base file to its target
// node, file and key.
func (g *gen) resolve(ref string) (*Node, string, string) { return g.resolveFrom(g.base, ref) }

func (g *gen) resolveFrom(base, ref string) (*Node, string, string) {
	file, frag, _ := strings.Cut(ref, "#")
	if file == "" {
		file = base
	} else {
		file = path.Clean(path.Join(path.Dir(base), file))
	}
	root := g.roots[file]
	if root == nil {
		return nil, "", ""
	}
	if frag == "" {
		return root, file, file + "#"
	}
	k, ok := strings.CutPrefix(frag, "/$defs/")
	if !ok || strings.Contains(k, "/") {
		return nil, "", ""
	}
	return root.Get("$defs").Get(k), file, file + "#/$defs/" + k
}

const helpersSrc = `// Code generated by internal/schemagen. DO NOT EDIT.

// SPDX-License-Identifier: Apache-2.0

package %s

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync/atomic"
)

// marshalTo encodes v on enc with enc's options: a union's MarshalJSONTo
// uses it so a strict encoder stays strict inside the union.
func marshalTo(enc *jsontext.Encoder, v any) error { return jsonv2.MarshalEncode(enc, v) }

// decodeStrict decodes exactly one document, refusing unknown fields and
// trailing data, so nested decoders are as strict as internal/contract.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the document")
	}
	return nil
}

// reasonCheck decides whether a reason code may be read (writing false:
// registered) or written (writing true: registered and not deprecated).
var reasonCheck atomic.Pointer[func(code string, writing bool) error]

// reasonCheckOwner is the import path of the only package that may install
// the reason check.
const reasonCheckOwner = %[2]q

// SetReasonCheck installs the reason check. Only the vocabulary package (an
// import path ending in /internal/vocab) may call it, once; any other caller,
// direct or indirect, and a second installation are refused. Until it is
// installed every reason is refused.
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

// funcPackage is the import path of a runtime function name
// (example.com/x/internal/vocab.init.0 → example.com/x/internal/vocab).
func funcPackage(name string) string {
	slash := strings.LastIndexByte(name, '/')
	if dot := strings.IndexByte(name[slash+1:], '.'); dot >= 0 {
		return name[:slash+1+dot]
	}
	return name
}

func checkReason(code string, writing bool) error {
	if code == "" {
		return errors.New("an empty reason code is never valid")
	}
	check := reasonCheck.Load()
	if check == nil {
		return errors.New("no reason vocabulary is loaded")
	}
	return (*check)(code, writing)
}
`

// declare emits the named type for a top-level schema or $defs entry.
func (g *gen) declare(name string, n *Node) {
	switch {
	case isStruct(n), isStringEnum(n):
		g.typeOf(n, name, name) // emits the declaration under this name
	case g.envelopes[name]:
		g.structType(n, name, name)
	case n.Get("oneOf") != nil:
		if t := g.typeOf(n, name, name); t != name { // a nullable union: alias of a pointer
			g.emit(name, name)
			g.doc(n, name)
			g.p("type %s = %s\n", name, t)
		}
	case isReasonString(n):
		g.reasonType(name, n)
	default:
		t := g.typeOf(n, name+"Value", name)
		g.emit(name, name)
		g.doc(n, name)
		if isCollectionType(t) {
			// A named collection is a defined type, never an alias, so any
			// recursion through collections (A = []A, A = []B with B = []A)
			// is representable: Go refuses alias cycles, not type cycles.
			g.p("type %s %s\n", name, t)
		} else {
			g.p("type %s = %s\n", name, t)
		}
	}
}

// isReasonString: a string schema with format adc-reason (a reason code).
func isReasonString(n *Node) bool {
	t, f := n.Get("type"), n.Get("format")
	return t != nil && t.Str == "string" && f != nil && f.Str == "adc-reason"
}

// reasonType emits a reason code as an opaque type: no string literal can
// become one, and it is read and written only through the installed check, however
// it is (un)marshalled. NameOf wraps a code unchecked, for the vocabulary's
// generated values alone.
func (g *gen) reasonType(name string, n *Node) {
	g.only(n, name, "string")
	g.emit(name, name)
	g.emit(name+"Of", name)
	g.doc(n, name)
	g.p(`// %[1]s is opaque: a zero value is never valid, and only the vocabulary's
// generated values, its parser, or decoding produce one.
type %[1]s struct{ code string }

// %[1]sOf wraps code without checking it. Only the vocabulary package calls
// it; adc dev vocab check refuses any other caller.
func %[1]sOf(code string) %[1]s { return %[1]s{code: code} }

// String returns the code.
func (r %[1]s) String() string { return r.code }

// MarshalText writes the code if the vocabulary accepts it for writing.
func (r %[1]s) MarshalText() ([]byte, error) {
	if err := checkReason(r.code, true); err != nil {
		return nil, err
	}
	return []byte(r.code), nil
}

// UnmarshalText reads a code the vocabulary accepts for reading.
func (r *%[1]s) UnmarshalText(b []byte) error {
	if err := checkReason(string(b), false); err != nil {
		return err
	}
	r.code = string(b)
	return nil
}

// UnmarshalJSON reads a JSON string through UnmarshalText; null is never a
// reason (encoding/json would otherwise skip UnmarshalText for it).
func (r *%[1]s) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return errors.New("a reason code cannot be null")
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	return r.UnmarshalText([]byte(s))
}
`, name)
	g.needs["encoding/json"], g.needs["errors"] = true, true
}

// typeOf returns the Go type for schema n, emitting any named declaration it
// needs under name. where is the location for errors.
func (g *gen) typeOf(n *Node, name, where string) string {
	if n == nil || n.Kind != Object {
		return g.fail(where, "a schema must be an object (boolean schemas and tuples are not supported)")
	}
	if ref := n.Get("$ref"); ref != nil {
		if refines(n) {
			// A refinement validates more but keeps the Go type of its target,
			// so the target must be a plain string schema.
			g.only(n, where, "refinement")
			if t := g.deref(n); t == nil || t.Get("type") == nil || t.Get("type").Str != "string" || t.Get("enum") != nil || t.Get("const") != nil {
				return g.fail(where, "a $ref with sibling keywords must name a string schema, which the siblings refine")
			}
		} else {
			g.only(n, where, "$ref")
		}
		_, _, key := g.resolve(ref.Str)
		if _, ok := g.names[key]; ok && g.deref(n) == nil {
			return g.fail(where, "$ref %s does not resolve (a $ref cycle)", ref.Str)
		}
		if gn, ok := g.names[key]; ok {
			if g.envelopes[gn] {
				return g.fail(where, "envelope %s may only be used as an allOf base", gn)
			}
			return gn
		}
		return g.fail(where, "$ref %s does not name a top-level type or $defs entry", ref.Str)
	}
	if one := n.Get("oneOf"); one != nil {
		g.only(n, where, "oneOf")
		if one.Kind != Array {
			return g.fail(where, "oneOf must be an array")
		}
		return g.union(n, one.Items, name, where)
	}
	if e := n.Get("enum"); e != nil {
		if t := n.Get("type"); t != nil && t.Str != "string" {
			return g.fail(where, "enum may only be of type string")
		}
		g.only(n, where, "enum")
		if !isStringEnum(n) || len(e.Items) == 0 {
			return g.fail(where, "enum must list strings")
		}
		g.enum(n, name, where)
		return name
	}
	if c := n.Get("const"); c != nil {
		g.only(n, where, "const")
		if t := n.Get("type"); c.Kind != String || (t != nil && t.Str != "string") {
			return g.fail(where, "only string const is supported")
		}
		return "string"
	}
	t := n.Get("type")
	if t == nil {
		return g.fail(where, "no type")
	}
	switch t.Str {
	case "string":
		g.only(n, where, "string")
		return "string"
	case "boolean":
		g.only(n, where, "boolean")
		return "bool"
	case "integer":
		g.only(n, where, "integer")
		lo, hi := n.Get("minimum"), n.Get("maximum")
		if lo == nil || hi == nil {
			return g.fail(where, "integer needs both minimum and maximum")
		}
		l, err1 := strconv.ParseInt(lo.Str, 10, 64)
		h, err2 := strconv.ParseInt(hi.Str, 10, 64)
		if err1 != nil || err2 != nil || l < -maxSafeInt || h > maxSafeInt {
			return g.fail(where, "integer bounds must lie within ±(2^53-1)")
		}
		return "int64"
	case "array":
		g.only(n, where, "array")
		items := n.Get("items")
		if items == nil {
			return g.fail(where, "array needs items")
		}
		return "[]" + g.typeOf(items, name+"Item", where+"/items")
	case "null":
		return g.fail(where, "null is supported only as a branch of a nullable oneOf")
	case "object":
		if n.Get("x-adc-open-payload") != nil {
			g.only(n, where, "open payload")
			g.needs["encoding/json"] = true
			return "json.RawMessage"
		}
		if ap := n.Get("additionalProperties"); ap != nil && ap.Kind == Object && n.Get("properties") == nil {
			g.only(n, where, "map")
			if pn := n.Get("propertyNames"); pn != nil {
				g.keyNames(pn, where+"/propertyNames")
			}
			return "map[string]" + g.typeOf(ap, name+"Value", where+"/additionalProperties")
		}
		if isStruct(n) {
			g.structType(n, name, where)
			return name
		}
		return g.fail(where, "open object without x-adc-open-payload")
	}
	return g.fail(where, "type %q", t.Str)
}

// isCollectionType: a Go slice or map type literal.
func isCollectionType(t string) bool {
	return strings.HasPrefix(t, "[]") || strings.HasPrefix(t, "map[")
}

// keyNames checks a propertyNames schema: a string schema, inline or by
// $ref, since map keys are Go strings.
func (g *gen) keyNames(n *Node, where string) {
	if n.Get("$ref") != nil {
		if !g.only(n, where, "$ref") {
			return
		}
		n = g.deref(n)
		if n == nil {
			g.fail(where, "propertyNames $ref does not name a schema")
			return
		}
	} else if !g.only(n, where, "key names") {
		return
	}
	t, e := n.Get("type"), n.Get("enum")
	switch {
	case isReasonString(n): // n is already resolved through any $ref chain
		g.fail(where, "a reason code cannot be a map key (its keys would be unchecked strings)")
	case t != nil && t.Str != "string", e != nil && !isStringEnum(n), t == nil && e == nil:
		g.fail(where, "propertyNames must be a string schema")
	}
}

// refines: a $ref with a keyword beside it that is not an annotation.
func refines(n *Node) bool {
	for _, k := range n.Keys {
		if k != "$ref" && !annotations[k] {
			return true
		}
	}
	return false
}

func isStringEnum(n *Node) bool {
	e := n.Get("enum")
	if e == nil {
		return false
	}
	for _, it := range e.Items {
		if it.Kind != String {
			return false
		}
	}
	return true
}

// isStruct: a closed object, or an allOf envelope closed by unevaluatedProperties.
func isStruct(n *Node) bool {
	if t := n.Get("type"); t == nil || t.Str != "object" {
		return false
	}
	if ap := n.Get("additionalProperties"); ap != nil && ap.Kind == Bool && !ap.Bool {
		return true
	}
	up := n.Get("unevaluatedProperties")
	return up != nil && up.Kind == Bool && !up.Bool && n.Get("allOf") != nil
}

// isEnvelope: an open object with properties (no additionalProperties or
// unevaluatedProperties of its own); closed by the struct that embeds it.
func isEnvelope(n *Node) bool {
	t := n.Get("type")
	return t != nil && t.Str == "object" && n.Get("properties") != nil &&
		n.Get("additionalProperties") == nil && n.Get("unevaluatedProperties") == nil
}

func goName(s string) string {
	var b strings.Builder
	isWord := func(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' }
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return !isWord(r) }) {
		switch part {
		case "id":
			b.WriteString("ID")
		default:
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String()
}

func (g *gen) enum(n *Node, name, where string) {
	g.emit(name, where)
	g.doc(n, name)
	g.p("type %s string\n\nconst (", name)
	for _, v := range n.Get("enum").Items {
		if goName(v.Str) == "" {
			g.fail(where, "enum value %q gives no Go constant name", v.Str)
		}
		g.emit(name+goName(v.Str), fmt.Sprintf("%s enum %q", where, v.Str))
		g.p("\t%s%s %s = %q", name, goName(v.Str), name, v.Str)
	}
	g.p(")\n")
}

// structType emits a closed struct, or an envelope (an open object used only
// as an allOf base, closed by the struct that embeds it).
func (g *gen) structType(n *Node, name, where string) {
	envelope := !isStruct(n)
	if envelope {
		g.only(n, where, "envelope")
	} else {
		g.only(n, where, "struct")
	}
	g.emit(name, where)
	var fields []string
	names := map[string]string{} // Go field name → JSON key, envelope fields included
	inherited := map[string]bool{}
	embeds := false
	if all := n.Get("allOf"); all != nil {
		for i, it := range all.Items {
			at := fmt.Sprintf("%s/allOf/%d", where, i)
			if ref := it.Get("$ref"); ref != nil && len(it.Keys) == 1 {
				target, _, key := g.resolve(ref.Str)
				env := g.names[key]
				if !g.envelopes[env] {
					g.fail(at, "an allOf $ref must name an envelope (open object with properties)")
					continue
				}
				embeds = true
				if prev, dup := names[env]; dup {
					g.fail(at, "envelope %s embedded twice (%s)", env, prev)
				}
				names[env] = "allOf " + env
				for _, k := range target.Get("properties").Keys {
					g.field(k, at, names)
					inherited[k] = true
				}
				fields = append(fields, "\t"+env)
			} else if it.Get("if") == nil {
				g.fail(at, "allOf entries must be a $ref envelope or an if/then dispatch")
			}
		}
	}
	ap, up := n.Get("additionalProperties"), n.Get("unevaluatedProperties")
	switch {
	case envelope:
	case embeds && (ap != nil || up == nil):
		// additionalProperties does not see properties that come in through
		// allOf: with it, every document carrying the envelope is invalid.
		g.fail(where, "an object embedding an allOf envelope must be closed by unevaluatedProperties:false alone, not additionalProperties")
	case !embeds && up != nil:
		g.fail(where, "unevaluatedProperties is supported only to close an object that embeds an allOf envelope; close others with additionalProperties:false")
	}
	props := n.Get("properties")
	if props != nil && props.Kind != Object {
		g.fail(where, "properties must be an object")
		return
	}
	var reasons []string // required reason fields, checked for presence on decode
	required := map[string]bool{}
	if r := n.Get("required"); r != nil {
		if r.Kind != Array {
			g.fail(where, "required must be an array")
		}
		for _, it := range r.Items {
			switch {
			case it.Kind != String:
				g.fail(where, "required must list strings")
			case required[it.Str]:
				g.fail(where, "required lists %q twice", it.Str)
			case props.Get(it.Str) == nil:
				g.fail(where, "required names %q, which is not in properties (the schema could never validate)", it.Str)
			}
			required[it.Str] = true
		}
	}
	if props != nil {
		for _, k := range props.Keys {
			at := where + "/properties/" + k
			if inherited[k] {
				g.fail(at, "property %q is already declared by the embedded envelope", k)
			}
			fname := g.field(k, at, names)
			prop := props.Get(k)
			typ := g.typeOf(prop, name+goName(k), at)
			tag := k
			switch {
			case required[k]:
				if typ == ReasonTypeName && !envelope {
					reasons = append(reasons, fname)
				}
			case strings.HasPrefix(typ, "*") || g.isNullable(prop, at):
				g.fail(at, "a nullable field must be required (null is explicit, absence invalid)")
			case g.isCollection(prop):
				// A pointer keeps absence (nil) apart from [] or {} (non-nil).
				typ, tag = "*"+typ, tag+",omitempty"
			case typ == "json.RawMessage":
				// Absent decodes to nil and is omitted; any present object
				// is at least "{}", so it survives omitempty.
				tag += ",omitempty"
			default:
				typ, tag = "*"+typ, tag+",omitempty"
			}
			doc := ""
			if d := prop.Get("description"); d != nil {
				doc = "\t// " + strings.ReplaceAll(d.Str, "\n", " ") + "\n"
			}
			fields = append(fields, fmt.Sprintf("%s\t%s %s `json:%q`", doc, fname, typ, tag))
		}
	}
	if n.Get("if") != nil || n.Get("then") != nil || n.Get("else") != nil {
		g.condition(n, props, where)
	}
	g.doc(n, name)
	g.p("type %s struct {\n%s\n}\n", name, strings.Join(fields, "\n"))
	if all := n.Get("allOf"); all != nil {
		g.accessors(all, props, required, names, name, where)
	}
	if len(reasons) > 0 {
		g.reasonPresence(name, reasons)
	}
}

// reasonPresence emits an UnmarshalJSON that refuses an omitted required
// reason code: encoding/json never calls the reason's own decoder for an
// absent field. It decodes into a fresh value, so an omitted field cannot
// keep a reason left in a reused destination.
func (g *gen) reasonPresence(name string, fields []string) {
	g.needs["encoding/json"], g.needs["errors"] = true, true
	g.p("\n// UnmarshalJSON decodes %s and refuses an omitted reason code.\n", name)
	g.p("func (x *%s) UnmarshalJSON(b []byte) error {\n\ttype plain %s\n\tvar p plain\n", name, name)
	g.p("\tif err := json.Unmarshal(b, &p); err != nil {\n\t\treturn err\n\t}\n")
	for _, f := range fields {
		g.p("\tif p.%s.code == \"\" {\n\t\treturn errors.New(%q)\n\t}\n", f, name+": a reason code is required ("+f+")")
	}
	g.p("\t*x = %s(p)\n\treturn nil\n}\n", name)
}

// reasonFormatOnlyDeclared refuses format adc-reason anywhere in n except a
// $defs/ReasonCode declaration of type string.
func reasonFormatOnlyDeclared(n *Node, at string) error {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case Object:
		if f := n.Fields["format"]; f != nil && f.Kind == String && f.Str == "adc-reason" {
			t := n.Fields["type"]
			if at != "/$defs/"+ReasonTypeName || t == nil || t.Str != "string" {
				return fmt.Errorf("%s: format adc-reason is allowed only on $defs/%s (type string); refer to it with $ref", at, ReasonTypeName)
			}
		}
		for _, k := range n.Keys {
			if err := reasonFormatOnlyDeclared(n.Fields[k], at+"/"+k); err != nil {
				return err
			}
		}
	case Array:
		for i, it := range n.Items {
			if err := reasonFormatOnlyDeclared(it, fmt.Sprintf("%s/%d", at, i)); err != nil {
				return err
			}
		}
	}
	return nil
}
