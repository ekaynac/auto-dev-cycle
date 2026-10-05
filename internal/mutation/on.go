// SPDX-License-Identifier: Apache-2.0

//go:build adcmutation

package mutation

import "os"

// Enabled reports whether this build can switch mutations on.
const Enabled = true

// Is reports whether ADC_MUTATION names this mutation.
func Is(name string) bool { return os.Getenv("ADC_MUTATION") == name }
