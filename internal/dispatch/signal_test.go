// SPDX-License-Identifier: Apache-2.0

package dispatch

import (
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// 5c-1's Unblock decides on a typed signal, one rule per kind beside its
// formatter: each placement and image signal reads back as the value that
// made it; a near miss is refused.
func TestThePlacementAndImageSignalsRoundTrip(t *testing.T) {
	for _, p := range []Placement{{"executor", "exe_01m4tm99haemqt8es2qfbwj8pa"}, {"profile", "prj_01m3250vkwffc979nzjw0g2sa1"}, {"destination", "stub"}, {"registry", ""}} {
		if got, ok := ParsePlacement(p.Signal()); !ok || got != p {
			t.Errorf("%+v: %q reads back %+v %v", p, p.Signal(), got, ok)
		}
	}
	for _, s := range []string{"", "placement:", "placement: ", "placement: executor", "placement: executor e", "placement: executor  exe_01m4tm99haemqt8es2qfbwj8pa",
		"placement: executor exe_01m4tm99haemqt8es2qfbwj8pa ", "placement: profile exe_01m4tm99haemqt8es2qfbwj8pa", "placement: host exe_01m4tm99haemqt8es2qfbwj8pa", "placement: registry x", "placement:executor exe_01m4tm99haemqt8es2qfbwj8pa",
		"Placement: executor exe_01m4tm99haemqt8es2qfbwj8pa", " placement: executor exe_01m4tm99haemqt8es2qfbwj8pa", "placement: destination Stub"} {
		if p, ok := ParsePlacement(s); ok {
			t.Errorf("%q reads as %+v", s, p)
		}
	}
	long := "registry.example/" + strings.Repeat("x", maxSignal)
	for _, ref := range []string{"docker.io/library/golang@sha256:" + strings.Repeat("a", 64), long} {
		if s := imageSignal(ref); !IsImageSignal(s) {
			t.Errorf("imageSignal(%.40q) = %.60q is not one", ref, s)
		}
	}
	for _, s := range []string{"", "image", "image ", "image a b", "image\tx", "images x", " image x", "image x\n", "image " + long,
		"image x", "image docker.io/library/golang@sha256:" + strings.Repeat("A", 64), "image sha256:" + strings.Repeat("a", 63)} {
		if IsImageSignal(s) {
			t.Errorf("%.60q is an image signal", s)
		}
	}
	// Opus #425 L2, Mythos L1: one rule with the profile. Every pin the
	// schema's PinnedImage admits, a vertical tab or another control
	// character in its name included, makes a signal IsImageSignal accepts,
	// so its wrong_owner hold is offered to its clearer; a name the schema
	// refuses (a space, a tab, a second @) makes none.
	digest := "@sha256:" + strings.Repeat("b", 64)
	for name, c := range map[string]struct {
		ref  string
		pins bool
	}{
		"a plain pin":    {"ghcr.io/x/y" + digest, true},
		"a vertical tab": {"reg/x\v" + digest, true},
		"a C0 control":   {"reg/x\x01" + digest, true},
		"a space":        {"reg/x y" + digest, false},
		"a tab":          {"reg/x\ty" + digest, false},
		"a second @":     {"reg/x@y" + digest, false},
		"no digest":      {"reg/x", false},
	} {
		if got := IsImageSignal(imageSignal(c.ref)); got != c.pins || sandbox.Pinned(c.ref) != c.pins {
			t.Errorf("%s: an image signal %v, pinned %v; want %v", name, got, sandbox.Pinned(c.ref), c.pins)
		}
	}
}
