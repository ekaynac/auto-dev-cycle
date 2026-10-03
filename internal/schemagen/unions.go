// SPDX-License-Identifier: Apache-2.0

package schemagen

import (
	"fmt"
	"go/format"
	"go/token"
	"sort"
	"strings"
)

// union handles the four oneOf forms of the grammar; anything else is refused.
func (g *gen) union(n *Node, items []*Node, name, where string) string {
	// Nullable: oneOf [null, X] → *X.
	if len(items) == 2 && (isNull(items[0]) || isNull(items[1])) {
		other := items[1]
		if isNull(items[1]) {
			other = items[0]
		}
		inner := g.typeOf(other, name+"Value", where+"/oneOf")
		if strings.HasPrefix(inner, "*") || g.isNullable(other, where+"/oneOf") {
			return g.fail(where, "nested nullable")
		}
		if isCollectionType(inner) {
			// A defined helper keeps A = *[]A representable.
			g.emit(name+"Value", where)
			g.p("type %sValue %s\n", name, inner)
			inner = name + "Value"
		}
		return "*" + inner
	}
	// Array or literal: oneOf [{const: L}, {type: array}].
	if len(items) == 2 {
		lit, arr := items[0], items[1]
		if lit.Get("const") == nil {
			lit, arr = arr, lit
		}
		if c := lit.Get("const"); c != nil && c.Kind == String && arr.Get("type") != nil && arr.Get("type").Str == "array" {
			if g.only(lit, where+"/oneOf", "const") {
				if t := lit.Get("type"); t != nil && t.Str != "string" {
					return g.fail(where+"/oneOf", "the literal branch must be a string const")
				}
			}
			g.arrayOrLiteral(n, name, c.Str, g.typeOf(arr, name+"List", where+"/oneOf"), where)
			return name
		}
	}
	// Resolve the branches to object schemas.
	var branches []*Node
	var names, bases []string
	for i, it := range items {
		b, bn, base := it, "", g.base
		if ref := it.Get("$ref"); ref != nil {
			if refines(it) {
				return g.fail(fmt.Sprintf("%s/oneOf/%d", where, i), "a union branch $ref may not have sibling keywords")
			}
			target, file, key := g.resolve(ref.Str)
			b, bn, base = target, g.names[key], file
		}
		if b == nil || !isStruct(b) {
			return g.fail(fmt.Sprintf("%s/oneOf/%d", where, i), "union branches must be closed objects")
		}
		if bn == "" && !g.only(b, fmt.Sprintf("%s/oneOf/%d", where, i), "struct") {
			return "invalid"
		}
		branches, names, bases = append(branches, b), append(names, bn), append(bases, base)
	}
	if disc := discriminator(branches); disc != "" {
		g.discriminated(n, name, disc, branches, names, where)
		return name
	}
	if keyed(branches) {
		g.keyedUnion(n, name, branches, bases, where)
		return name
	}
	return g.fail(where, "oneOf is neither nullable, array-or-literal, const-discriminated nor keyed")
}

// isNull: {"type":"null"}, annotations allowed.
func isNull(n *Node) bool {
	t := n.Get("type")
	if t == nil || t.Kind != String || t.Str != "null" {
		return false
	}
	for _, k := range n.Keys {
		if k != "type" && !annotations[k] {
			return false
		}
	}
	return true
}

// discriminator: a property that every branch requires as a distinct string const.
func discriminator(branches []*Node) string {
	props := map[string]int{}
	for _, b := range branches {
		for _, k := range b.Get("required").Strings() {
			if c := b.Get("properties").Get(k).Get("const"); c != nil && c.Kind == String {
				props[k]++
			}
		}
	}
	var cands []string
	for k, c := range props {
		if c == len(branches) {
			cands = append(cands, k)
		}
	}
	sort.Strings(cands)
	for _, k := range cands {
		seen := map[string]bool{}
		for _, b := range branches {
			seen[b.Get("properties").Get(k).Get("const").Str] = true
		}
		if len(seen) == len(branches) {
			return k
		}
	}
	return ""
}

// keyed: each branch is a closed object with exactly one property, which it
// requires, and those keys are disjoint. A branch with anything else (an
// embedded envelope, a condition) is not keyed: its other contents would be
// dropped by the one-field-per-branch type.
func keyed(branches []*Node) bool {
	seen := map[string]bool{}
	for _, b := range branches {
		for _, k := range b.Keys {
			if !shapeKeys["keyed branch"][k] && !annotations[k] {
				return false
			}
		}
		req := b.Get("required").Strings()
		if len(req) != 1 || len(b.Get("properties").Keys) != 1 || seen[req[0]] {
			return false
		}
		seen[req[0]] = true
	}
	return true
}

func (g *gen) discriminated(n *Node, name, disc string, branches []*Node, names []string, where string) {
	g.needs["encoding/json"], g.needs["fmt"] = true, true
	g.emit(name, where)
	g.emit(name+"Variant", where)
	g.doc(n, name)
	g.p("type %s struct{ Variant %sVariant }\n", name, name)
	var impl []string
	for i, b := range branches {
		if names[i] == "" {
			names[i] = name + goName(b.Get("properties").Get(disc).Get("const").Str)
			g.structType(b, names[i], fmt.Sprintf("%s/oneOf/%d", where, i))
		}
		impl = append(impl, "*"+names[i])
	}
	g.p("// %sVariant is implemented by %s.", name, strings.Join(impl, ", "))
	g.p("type %sVariant interface{ is%s() }\n", name, name)
	for _, v := range names {
		g.p("func (*%s) is%s() {}", v, name)
	}
	g.p(`
func (u %[1]s) MarshalJSON() ([]byte, error) {
	if u.Variant == nil {
		return nil, fmt.Errorf("%[1]s: no variant set")
	}
	return json.Marshal(u.Variant)
}

func (u *%[1]s) UnmarshalJSON(b []byte) error {
	// The discriminator is read by its exact key: encoding/json matches
	// struct fields case-insensitively.
	var head map[string]json.RawMessage
	if err := json.Unmarshal(b, &head); err != nil {
		return err
	}
	raw, ok := head[%[2]q]
	if !ok {
		return fmt.Errorf("%[1]s: no %[2]s")
	}
	var d string
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	switch d {`, name, disc)
	for i, b := range branches {
		g.p("\tcase %q:\n\t\tv := &%s{}\n\t\tif err := decodeStrict(b, v); err != nil {\n\t\t\treturn err\n\t\t}\n\t\tu.Variant = v",
			b.Get("properties").Get(disc).Get("const").Str, names[i])
	}
	g.p("\tdefault:\n\t\treturn fmt.Errorf(\"%s: unknown %s %%q\", d)\n\t}\n\treturn nil\n}\n", name, disc)
}

func (g *gen) keyedUnion(n *Node, name string, branches []*Node, bases []string, where string) {
	g.emit(name, where)
	var fields []string
	seen := map[string]string{}
	for i, b := range branches {
		k := b.Get("required").Strings()[0]
		f := g.field(k, where+"/"+k, seen)
		prop := b.Get("properties").Get(k)
		var typ string
		var nullable bool
		g.within(bases[i], func() {
			typ = g.typeOf(prop, name+goName(k), where+"/"+k)
			nullable = g.isNullable(prop, where+"/"+k)
		})
		if strings.HasPrefix(typ, "*") || nullable {
			g.fail(where+"/"+k, "a keyed-union branch may not be nullable (it would decode as a double pointer)")
		}
		fields = append(fields, fmt.Sprintf("\t%s *%s `json:%q`", f, typ, k+",omitempty"))
	}
	g.doc(n, name)
	g.p("// Exactly one field is set; the schema enforces it.\ntype %s struct {\n%s\n}\n", name, strings.Join(fields, "\n"))
}

func (g *gen) arrayOrLiteral(n *Node, name, lit, list, where string) {
	g.needs["bytes"], g.needs["encoding/json"], g.needs["fmt"] = true, true, true
	g.emit(name, where)
	field := goName(lit)
	if field == "" {
		field = "All" // a non-word literal such as "*"
	}
	if field == "List" || field == "MarshalJSON" || field == "UnmarshalJSON" || !token.IsIdentifier(field) || !token.IsExported(field) {
		g.fail(where, "literal %q gives no usable Go field name next to List and the generated methods", lit)
	}
	g.doc(n, name)
	g.p(`// Either %[2]s is true (the literal %[3]q) or List holds the items.
type %[1]s struct {
	%[2]s bool
	List %[4]s
}

func (v %[1]s) MarshalJSON() ([]byte, error) {
	if v.%[2]s {
		return json.Marshal(%[3]q)
	}
	return json.Marshal(v.List)
}

func (v *%[1]s) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return fmt.Errorf("%[1]s: null is neither %%q nor a list", %[3]q)
	}
	if bytes.HasPrefix(bytes.TrimSpace(b), []byte(`+"`\"`"+`)) {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s != %[3]q {
			return fmt.Errorf("%[1]s: unexpected literal %%q", s)
		}
		*v = %[1]s{%[2]s: true}
		return nil
	}
	*v = %[1]s{}
	return decodeStrict(b, &v.List)
}
`, name, field, lit, list)
}

// accessors emits a typed accessor per if/then dispatch entry of a struct's
// allOf: if <disc> is const V then <field> is $ref D. The if must require
// disc (an if without required is vacuously true when disc is absent), disc
// and field must be required properties of the struct, disc a string, and
// there is no else (the if/then allow-list refuses it).
func (g *gen) accessors(all, props *Node, required map[string]bool, fields map[string]string, name, where string) {
	methods := map[string]bool{}
	for i, it := range all.Items {
		cond, then := it.Get("if"), it.Get("then")
		if cond == nil {
			continue
		}
		at := fmt.Sprintf("%s/allOf/%d", where, i)
		if !selects(then) {
			if g.only(it, at, "conditional") {
				g.condition(it, props, at)
			}
			continue
		}
		if !g.only(it, at, "if/then") || !g.only(cond, at+"/if", "if") {
			return
		}
		if then == nil || !g.only(then, at+"/then", "then") ||
			len(cond.Get("properties").Keys) != 1 || len(then.Get("properties").Keys) != 1 {
			g.fail(at, "if/then dispatch must test one const property and select one $ref")
			return
		}
		disc := cond.Get("properties").Keys[0]
		if req := cond.Get("required").Strings(); len(req) != 1 || req[0] != disc {
			g.fail(at, `the if must have "required": [%q]; without it the if holds whenever %s is absent`, disc, disc)
			return
		}
		test := cond.Get("properties").Get(disc)
		val := test.Get("const")
		if !g.only(test, at+"/if/properties/"+disc, "const") || val == nil || val.Kind != String ||
			(test.Get("type") != nil && test.Get("type").Str != "string") {
			g.fail(at, "the if must test %s against one string const", disc)
			return
		}
		discProp := g.deref(props.Get(disc))
		if !required[disc] || discProp == nil {
			g.fail(at, "the dispatch property %s must be a required property of the object", disc)
			return
		}
		if e := discProp.Get("enum"); e != nil {
			if !containsString(e, val.Str) {
				g.fail(at, "%q is not a value of %s", val.Str, disc)
				return
			}
		} else if t := discProp.Get("type"); discProp.Get("const") == nil && (t == nil || t.Str != "string") {
			g.fail(at, "the dispatch property %s must be a string", disc)
			return
		}
		field := then.Get("properties").Keys[0]
		sel := then.Get("properties").Get(field)
		ref := sel.Get("$ref")
		if ref == nil || !g.only(sel, at+"/then/properties/"+field, "$ref") || props.Get(field).Get("x-adc-open-payload") == nil {
			g.fail(at, "if/then dispatch must select a $ref for an x-adc-open-payload field")
			return
		}
		if !required[field] {
			g.fail(at, "the dispatched field %s must be required", field)
			return
		}
		_, _, key := g.resolve(ref.Str)
		target := g.names[key]
		if target == "" || g.envelopes[target] {
			g.fail(at, "$ref %s does not name a top-level type or $defs entry", ref.Str)
			return
		}
		method := goName(field) + "As" + target
		if _, clash := fields[method]; clash || methods[method] {
			g.fail(at, "accessor %s collides with a field or another accessor", method)
			return
		}
		methods[method] = true
		g.needs["fmt"] = true
		g.p(`// %[3]sAs%[5]s decodes %[3]s as %[5]s; only valid when %[1]s is %[4]q.
func (x *%[6]s) %[3]sAs%[5]s() (%[5]s, error) {
	var out %[5]s
	if string(x.%[2]s) != %[4]q {
		return out, fmt.Errorf("%[6]s.%[3]s is a %[5]s only when %[1]s is %%q, not %%q", %[4]q, x.%[2]s)
	}
	return out, decodeStrict(x.%[3]s, &out)
}
`, disc, goName(disc), goName(field), val.Str, target, name)
	}
}

// selects: a then that names a $ref for a property, i.e. a dispatch entry
// rather than a validation-only condition.
func selects(then *Node) bool {
	props := then.Get("properties")
	if props == nil {
		return false
	}
	for _, k := range props.Keys {
		if props.Get(k).Get("$ref") != nil {
			return true
		}
	}
	return false
}

// condition accepts an if/then/else that only constrains properties the
// object declares (const, enum, not, length, pattern, bounds), so the Go
// shape is the same whichever branch holds; internal/contract validates it.
func (g *gen) condition(n, props *Node, where string) {
	if n.Get("if") == nil || n.Get("then") == nil {
		g.fail(where, "a condition needs both if and then")
		return
	}
	for _, part := range []string{"if", "then", "else"} {
		s := n.Get(part)
		if s == nil {
			continue
		}
		at := where + "/" + part
		if !g.only(s, at, "condition") {
			return
		}
		for _, k := range s.Get("required").Strings() {
			if props.Get(k) == nil {
				g.fail(at, "required names %q, which is not a property of the object", k)
			}
		}
		cprops := s.Get("properties")
		if cprops == nil {
			continue
		}
		for _, k := range cprops.Keys {
			c := cprops.Get(k)
			if props.Get(k) == nil {
				g.fail(at, "%q is not a property of the object", k)
				continue
			}
			g.constraint(c, at+"/properties/"+k)
		}
	}
}

// constraint checks a validation-only property schema, following not to
// any depth.
func (g *gen) constraint(c *Node, where string) {
	for ; c != nil; c, where = c.Get("not"), where+"/not" {
		if !g.only(c, where, "constraint") {
			return
		}
	}
}

func containsString(arr *Node, s string) bool {
	for _, it := range arr.Items {
		if it.Kind == String && it.Str == s {
			return true
		}
	}
	return false
}

// finish assembles the file: header, imports, body, gofmt.
func (g *gen) finish(pkg string) ([]byte, error) {
	var src strings.Builder
	fmt.Fprintf(&src, "// Code generated by internal/schemagen from schemas/%s. DO NOT EDIT.\n\n// SPDX-License-Identifier: Apache-2.0\n\npackage %s\n\n", g.file, pkg)
	if len(g.needs) > 0 {
		imps := make([]string, 0, len(g.needs))
		for i := range g.needs {
			imps = append(imps, fmt.Sprintf("%q", i))
		}
		sort.Strings(imps)
		fmt.Fprintf(&src, "import (\n%s\n)\n\n", strings.Join(imps, "\n"))
	}
	src.WriteString(g.buf.String())
	out, err := format.Source([]byte(src.String()))
	if err != nil {
		// Name the generated line: a format error is a generator defect.
		var line int
		if _, scanErr := fmt.Sscanf(err.Error(), "%d:", &line); scanErr == nil {
			if lines := strings.Split(src.String(), "\n"); line > 0 && line <= len(lines) {
				return nil, fmt.Errorf("%w\n  generated line %d: %s", err, line, lines[line-1])
			}
		}
		return nil, err
	}
	return out, nil
}
