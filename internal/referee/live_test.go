// SPDX-License-Identifier: Apache-2.0

package referee

// The live row judges a T0 item under the real rootless Podman, which the
// test sandbox (bubblewrap) does not offer, so it runs only when asked,
// outside it, after the items' repositories are built:
//
//	fixtures/m1/referee-items/build-repos.sh
//	ADC_REFEREE_LIVE=1 go test -run Live ./internal/referee/
//
// It uses the user's Podman storage, pulling the item's pinned image when
// it is absent.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLiveT0Item(t *testing.T) {
	if os.Getenv("ADC_REFEREE_LIVE") != "1" {
		t.Skip("set ADC_REFEREE_LIVE=1 to run the live Podman row")
	}
	podman, err := exec.LookPath("podman")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.Abs("../../fixtures/m1/referee-items")
	if err != nil {
		t.Fatal(err)
	}
	item, err := os.ReadFile(filepath.Join(dir, "go-mini-bugfix-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	patch, err := os.ReadFile("../../fixtures/t0/go-mini/tasks/go-mini-bugfix-1/reference.patch")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		patch []byte
		want  Verdict
	}{{patch, Accepted}, {nil, Rejected}} {
		r := Judge(context.Background(), In{Podman: podman, Item: item, ItemDir: dir, Patch: row.patch})
		if r.Verdict != row.want || r.Runs["base"] == nil || r.Runs["base"].Passed == 0 {
			t.Fatalf("got %+v, want %s", r, row.want)
		}
	}
}
