// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

const clean = "iface lo\nroutes 0\n"

// The probe's verdict goes red under each F fault the run could suffer:
// a network, a controller path, a controller socket, a HOME with content.
func TestProbeVerdict(t *testing.T) {
	cases := []struct {
		name string
		mode Mode
		out  string
		want []string // violations, by prefix; nil is a pass
	}{
		{"gate, contained", Gate, clean + "done\n", nil},
		{"build, gateway only", Build, clean + "socket " + GatewaySocket + "\ndone\n", nil},
		{"a network interface", Gate, "iface lo\niface eth0\nroutes 0\ndone\n", []string{"network interface eth0"}},
		{"a route", Gate, "iface lo\nroutes 2\ndone\n", []string{"a network route"}},
		{"the state directory", Gate, clean + "present /srv/adc/state\ndone\n", []string{"controller path present: /srv/adc/state"}},
		{"HOME has content", Gate, clean + "home .netrc\ndone\n", []string{"HOME is not empty"}},
		{"gate with the gateway socket", Gate, clean + "socket " + GatewaySocket + "\ndone\n", []string{"socket reachable"}},
		{"build with adcd's socket too", Build, clean + "socket " + GatewaySocket + "\nsocket /mnt/adcd.sock\ndone\n", []string{"socket reachable: /mnt/adcd.sock"}},
		{"build without the gateway", Build, clean + "done\n", []string{"the gateway socket is absent"}},
	}
	for _, c := range cases {
		r, err := parse([]byte(c.out))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		r.Mode = c.mode
		got := r.check()
		if len(got) != len(c.want) {
			t.Errorf("%s: violations %q, want %q", c.name, got, c.want)
			continue
		}
		for i, w := range c.want {
			if !strings.HasPrefix(got[i], w) {
				t.Errorf("%s: violation %q, want %q", c.name, got[i], w)
			}
		}
	}
}

// A probe that did not run to its end proves nothing.
func TestProbeIncomplete(t *testing.T) {
	for _, out := range []string{"", clean, "unreadable\n" + clean + "done\n", "routes x\ndone\n", clean + "surprise\ndone\n"} {
		if _, err := parse([]byte(out)); !errors.Is(err, ErrProbe) {
			t.Errorf("%q: %v, want ErrProbe", out, err)
		}
	}
}

// Probe runs the fixed script in a fresh attempt, looks for every
// forbidden path and the controller's home, and tears the attempt down.
func TestProbeRuns(t *testing.T) {
	f := &fake{fn: func(context.Context, []string) ([]byte, error) { return []byte(clean + "done\n"), nil }}
	h := testHost(t, f)
	r, err := h.Probe(context.Background(), Image{Pinned: "x@sha256:" + testID, ID: testID}, Gate, "", "/home/op")
	if err != nil || len(r.Violations) != 0 || r.Mode != Gate {
		t.Fatalf("Probe = %+v, %v", r, err)
	}
	args := f.calls[0]
	tail := args[slices.Index(args, probeScript)+2:]
	if !slices.Equal(tail, append(slices.Clone(h.Forbidden), "/home/op")) {
		t.Errorf("probe looked for %q, want the forbidden paths and the home", tail)
	}
	if runs, _ := listDir(h.Root + "/runs"); len(runs) != 0 {
		t.Errorf("the probe's attempt was not torn down: %q", runs)
	}
}
