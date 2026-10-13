// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

// clean is what a contained gate run reports (from a live run, trimmed).
const clean = `iface lo
routes 0
mount / / overlay
mount /proc / proc
mount /workspace /tmp/r/runs/a1/workspace ext4
mount /results /tmp/r/runs/a1/results ext4
mount /run/.containerenv /tmp/r/r/overlay-containers/x/userdata/.containerenv ext4
mount /etc/hostname /tmp/r/r/overlay-containers/x/userdata/hostname ext4
mount /dev/null /null devtmpfs
mount /proc/sys /sys proc
mount /tmp / tmpfs
status CapEff 0000000000000000
status NoNewPrivs 1
status Seccomp 2
limit memory 268435456
limit pids 64
`

// The probe's verdict goes red under each F fault the run could suffer:
// a network, a controller path or mount, a controller socket, a HOME with
// content, capabilities, no seccomp, limits not in force.
func TestProbeVerdict(t *testing.T) {
	gw := "mount " + GatewaySocket + " /tmp/gw/gw.sock ext4\nsocket " + GatewaySocket + "\n"
	cases := []struct {
		name string
		mode Mode
		out  string
		want []string // violations, by prefix; nil is a pass
	}{
		{"gate, contained", Gate, clean, nil},
		{"build, gateway only", Build, clean + gw, nil},
		{"a network interface", Gate, clean + "iface eth0\n", []string{"network interface eth0"}},
		{"a route", Gate, clean + "routes 2\n", []string{"a network route"}},
		{"the state directory", Gate, clean + "present /srv/adc/state\n", []string{"controller path present: /srv/adc/state"}},
		{"a host bind elsewhere", Gate, clean + "mount /mnt/x /srv/adc/state ext4\n", []string{"a mount from the host: /mnt/x"}},
		{"a host filesystem whole", Gate, clean + "mount /mnt/x / ext4\n", []string{"a mount from the host: /mnt/x"}},
		{"a bind posing as Podman's", Gate, clean + "mount /etc/hosts /home/op/.ssh/known_hosts ext4\n", []string{"a mount from the host: /etc/hosts"}},
		{"HOME has content", Gate, clean + "home .netrc\n", []string{"HOME is not empty"}},
		{"capabilities", Gate, clean + "status CapEff 000001ffffffffff\n", []string{"CapEff is 000001ffffffffff"}},
		{"no seccomp", Gate, clean + "status Seccomp 0\n", []string{"Seccomp is 0"}},
		{"no_new_privs off", Gate, clean + "status NoNewPrivs 0\n", []string{"NoNewPrivs is 0"}},
		{"no pids limit", Gate, clean + "limit pids max\n", []string{"the pids limit is max"}},
		{"no memory limit", Gate, clean + "limit memory max\n", []string{"the memory limit is max"}},
		{"gate with the gateway mounted", Gate, clean + gw, []string{"a mount from the host: " + GatewaySocket, "socket reachable"}},
		{"build with adcd's socket too", Build, clean + gw + "socket /mnt/adcd.sock\n", []string{"socket reachable: /mnt/adcd.sock"}},
		{"build without the gateway", Build, clean, []string{"the gateway socket is absent"}},
	}
	for _, c := range cases {
		r, err := parse([]byte(c.out + "done\n"))
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

// A probe that did not run to its end proves nothing; neither does one
// that did not report its status or limits.
func TestProbeIncomplete(t *testing.T) {
	for _, out := range []string{"", clean, "unreadable\n" + clean + "done\n", "routes x\ndone\n",
		clean + "surprise\ndone\n", clean + "4747 2821 0:287 / / ro\ndone\n"} {
		if _, err := parse([]byte(out)); !errors.Is(err, ErrProbe) {
			t.Errorf("%q: %v, want ErrProbe", out, err)
		}
	}
	r, err := parse([]byte("iface lo\nroutes 0\ndone\n"))
	if err != nil || len(r.check()) == 0 {
		t.Errorf("a report without status or limits passed: %v", err)
	}
}

// Probe runs the fixed script in a fresh attempt, looks for every
// forbidden path and the controller's home, and tears the attempt down.
func TestProbeRuns(t *testing.T) {
	f := &fake{fn: func(context.Context, []string) ([]byte, error) { return []byte(clean + "done\n"), nil }}
	h := testHost(t, f)
	r, err := h.Probe(context.Background(), Image{Pinned: "x@sha256:" + testID, ID: testID}, Gate, "")
	if err != nil || len(r.Violations) != 0 || r.Mode != Gate {
		t.Fatalf("Probe = %+v, %v", r, err)
	}
	args := f.calls[0]
	tail := args[slices.Index(args, probeScript)+2:]
	if !slices.Equal(tail, append(slices.Clone(h.Forbidden), h.Home)) {
		t.Errorf("probe looked for %q, want the forbidden paths and the home", tail)
	}
	if runs, _ := os.ReadDir(h.Root + "/runs"); len(runs) != 0 {
		t.Errorf("the probe's attempt was not torn down: %v", runs)
	}
}
