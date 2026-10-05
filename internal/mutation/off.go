// SPDX-License-Identifier: Apache-2.0

//go:build !adcmutation

package mutation

// Enabled reports whether this build can switch mutations on.
const Enabled = false

// Is is always false in an ordinary build.
func Is(string) bool { return false }
