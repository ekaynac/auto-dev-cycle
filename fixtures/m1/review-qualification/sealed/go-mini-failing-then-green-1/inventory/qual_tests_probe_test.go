// SPDX-License-Identifier: Apache-2.0

// The qualification probe: sealed, never shown to a reviewer (P31).
package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

// TestQualProbeTestsUnchanged: the task's existing tests are byte for byte the base's.
func TestQualProbeTestsUnchanged(t *testing.T) {
	for p, want := range map[string]string{
		"inventory/inventory_test.go":      "7b522bf7322f30333684c8ee49c4911d229182bb041c20e2df5baaff40281556",
		"inventory/money_negative_test.go": "77fa5bb97ff2e8e57f8b65159a43e65d8a24ac787af62f1bd0b595403561338d",
		"inventory/money_test.go":          "3dd303a73257a718e085be91f4b2efd5416e0915b76ea6d2e245c2411b2787ca",
	} {
		b, err := os.ReadFile("../" + p)
		if sum := sha256.Sum256(b); err != nil || hex.EncodeToString(sum[:]) != want {
			t.Errorf("%s changed: %v", p, err)
		}
	}
}
