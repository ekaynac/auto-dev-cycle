// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"slices"
	"strings"
)

// P14b-out-2 (split from #276 under 04 §2): in the additive layer, with its
// own work bounds, warnings only. A changed body, variable initializer or
// deferred literal that uses S by its text warns once per file and package,
// though its references stay the same. (References by types, changed method
// values among them, are P14b-out-3's: #280 round 3.)

// unit is one part of a declaration the layer reads: its references are
// counted under where, as P14b-out-1 counts them (its blocking decisions
// keep that identity: Astra #280 H1); its source spans are fingerprinted
// under bind ("": none), the identity of the additive warnings only.
type unit struct {
	n           ast.Node
	where, bind string
	lits        bool // only its function literals: they run later
}

// units are d's parts: a function, bound by its bounded name; each var spec,
// bound by all its names (blank ones too: Astra #276 r3 #5) as one bounded
// identity, computed once, its bytes charged (Astra #280 H4); the function
// literals of initialization and of the real TestMain(*testing.M) hook of a
// _test.go file (r3 #4), bound as one; another declaration, unbound. Names
// are bounded once here, their original bytes charged (r3 #6).
func (r *run) units(d ast.Decl, p, testing string) []unit {
	switch d := d.(type) {
	case *ast.FuncDecl:
		switch {
		case d.Recv == nil && (d.Name.Name == "init" || strings.HasSuffix(p, "_test.go") && d.Name.Name == "TestMain" && hook(d, testing)):
			return []unit{{d, "init literal", "init literal", true}}
		case d.Recv != nil && r.charge(len(d.Name.Name)+len(recv(d)), 0):
			w := bound(recv(d) + "." + d.Name.Name)
			return []unit{{d, w, "func " + w, false}}
		case d.Recv == nil && r.charge(len(d.Name.Name), 0):
			return []unit{{d, bound(d.Name.Name), "func " + bound(d.Name.Name), false}}
		}
	case *ast.GenDecl:
		if d.Tok == token.VAR {
			var us []unit
			for _, sp := range d.Specs {
				var ns []string
				for _, id := range sp.(*ast.ValueSpec).Names {
					if r.charge(len(id.Name)+2, 0) {
						ns = append(ns, id.Name)
					}
				}
				us = append(us, unit{sp, "package level", "var " + bound(strings.Join(ns, ", ")), false})
			}
			return us
		}
	}
	return []unit{{d, "package level", "", false}}
}

// print combines u's spans (its function literals only, with lits) into its
// binding's fingerprint, sha256(prev||span) in source order: declarations
// sharing a binding (two func _, a bound name's collision) never overwrite.
func (r *run) print(of *outFile, src []byte, u unit) {
	spans := []ast.Node{u.n}
	if u.bind == "" {
		return
	} else if u.lits {
		spans = nil
		ast.Inspect(u.n, func(n ast.Node) bool {
			if l, ok := n.(*ast.FuncLit); ok {
				spans = append(spans, l)
			}
			_, lit := n.(*ast.FuncLit)
			return n == nil || !lit && r.charge(1, 0)
		})
	}
	tf := r.out.fset.File(of.f.Pos())
	for _, n := range spans {
		s := src[tf.Offset(n.Pos()):tf.Offset(n.End())]
		if !r.charge(len(s), 0) {
			return
		}
		h := sha256.Sum256(append([]byte(of.prints[u.bind]), s...))
		of.prints[u.bind] = string(h[:])
	}
}

func (of *outFile) use(bind, pkg string) {
	if bind != "" {
		if of.uses[bind] == nil {
			of.uses[bind] = map[string]bool{}
		}
		of.uses[bind][pkg] = true
	}
}

// outBodies warns, once per file and package, for the bindings whose
// fingerprint changed, or that are added or gone (but a function's: its
// references move), that use S in either version, unless the references
// warning for that file and package names it already.
func (r *run) outBodies(b, a outFacts, changed map[string][]string) {
	bodies := map[string][]string{}
	for _, k := range keys(b.prints, a.prints) {
		file, bind, _ := strings.Cut(k, "\x00")
		bp, inB := b.prints[k]
		if ap, inA := a.prints[k]; inB && inA && bp == ap || (!inB || !inA) && strings.HasPrefix(bind, "func ") {
			continue // unchanged; a function added or gone moves its references (a var spec's or a literal's do not: Opus #280 M3)
		}
		for _, pkg := range slices.Sorted(maps.Keys(maps.Collect(func(yield func(string, bool) bool) {
			for _, m := range []map[string]bool{b.uses[k], a.uses[k]} {
				for p := range m {
					yield(p, true)
				}
			}
		}))) {
			if !slices.Contains(changed[file+"\x00"+pkg], strings.TrimPrefix(bind, "func ")) { // its references' warning names it already
				bodies[file+"\x00"+pkg] = append(bodies[file+"\x00"+pkg], bind)
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(bodies)) {
		file, pkg, _ := strings.Cut(k, "\x00")
		bs := bodies[k]
		r.outWarn(file, fmt.Sprintf("changed bodies that use %s (%d: %s%s): review it", pkg, len(bs),
			strings.Join(bs[:min(3, len(bs))], ", "), map[bool]string{true: ", …"}[len(bs) > 3]))
	}
}
