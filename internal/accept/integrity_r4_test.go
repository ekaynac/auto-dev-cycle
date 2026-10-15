// SPDX-License-Identifier: Apache-2.0

package accept

import "testing"

// F (P14e-2b2, #226 entry 85; Mythos #271 r2 L1 and L2, r3's list): each
// row reproduced first on main (0ae4e77).
func TestIntegrity2b2(t *testing.T) {
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"help/help.go": "package help\n\nfunc One() int { return 1 }\n", "pkg/testdata/x.json": "{}\n"})
		plain(t, src, "add", "--all")
	})
	// r2 L1: a numeric type parameter converted to uintptr is no raw address;
	// one with an unsafe.Pointer or pointer term still is.
	for name, c := range map[string]struct{ src, why string }{
		"L1 N ~int | ~uintptr to uintptr":            {"package help\n\nfunc U[N ~int | ~uintptr](n N) uintptr { return uintptr(n) }\n", ""},
		"L1 N ~unsafe.Pointer | ~uintptr to uintptr": {"package help\n\nimport \"unsafe\"\n\nfunc U[N ~unsafe.Pointer | ~uintptr](n N) uintptr { return uintptr(n) }\n", "can forge"},
		"L1 N *int | ~uintptr to uintptr":            {"package help\n\nfunc U[N *int | ~uintptr](n N) uintptr { return uintptr(n) }\n", "can forge"},
		"L1 a named constraint with a pointer term":  {"package help\n\nimport \"unsafe\"\n\ntype raw interface{ ~unsafe.Pointer }\n\nfunc U[N raw](n N) uintptr { return uintptr(n) }\n", "can forge"},
	} {
		if v := f.guarded(t, map[string]string{"help/u.go": c.src}); c.why == "" && (!v.Accepted() || len(v.Warnings) > 0) || c.why != "" && !finds(v.Blocks, c.why) {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}

	// Item 1: SWIG turns cgo on by itself: a .swig file in either version
	// makes the repository one that uses cgo.
	for name, files := range map[string]map[string]string{
		"a .swig in the base, testdata changed":     {"pkg/testdata/x.json": "[]\n"},
		"a .swigcxx in the candidate, testdata too": {"help/y.swigcxx": "%module y\n", "pkg/testdata/x.json": "[]\n"},
	} {
		base := f
		if _, ok := files["help/y.swigcxx"]; !ok {
			base = newFixtureWith(t, func(src string) {
				write(t, src, guardBase)
				write(t, src, map[string]string{"help/x.swig": "%module x\n", "pkg/testdata/x.json": "{}\n"})
				plain(t, src, "add", "--all")
			})
		}
		if v := base.guarded(t, files); !finds(v.Blocks, "a non-Go file in a repository that uses cgo") {
			t.Errorf("%s: %q", name, whats(v.Blocks))
		}
	}

	// r2 L2: the cgo pass reads with its own bound, not the guard's budget
	// (a Go change typed within the budget the pass would have spent), and
	// fails closed past it.
	big := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"other/a.go": "package other\n\n// " + pad(6000) + "\n", "pkg/testdata/x.json": "{}\n"})
		plain(t, src, "add", "--all")
	})
	oldRead := maxRead
	maxRead = 6000 // the pkg files, not other/a.go too
	v := big.guarded(t, map[string]string{"pkg/x.go": "package pkg\n\nfunc G() int { return 1 }\n", "pkg/testdata/x.json": "[]\n"})
	maxRead = oldRead
	if !v.Accepted() {
		t.Errorf("L2 the cgo pass within its own bound: %q", whats(v.Blocks))
	}
	oldCgo := maxCgoRead
	maxCgoRead = 10
	v = big.guarded(t, map[string]string{"pkg/testdata/x.json": "[]\n"})
	maxCgoRead = oldCgo
	if !finds(v.Blocks, "a non-Go file in a repository that uses cgo") {
		t.Errorf("L2 the cgo pass past its own bound: %q", whats(v.Blocks))
	}
}

func pad(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}
