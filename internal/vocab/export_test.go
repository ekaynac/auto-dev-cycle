// SPDX-License-Identifier: Apache-2.0

package vocab

import "github.com/ekaynac/auto-dev-cycle/gen/go/types"

// DeprecateForTest marks code deprecated in the authoritative registry (the
// one every decision reads) until the returned function restores it. It
// exists only in test builds.
func DeprecateForTest(code string, since types.Semver) (restore func()) {
	reg, err := authoritative()
	if err != nil {
		panic(err)
	}
	orig := reg.Reasons[code]
	dep := orig
	dep.Deprecated = &since
	reg.Reasons[code] = dep
	return func() { reg.Reasons[code] = orig }
}
