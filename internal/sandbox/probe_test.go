// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testGateway = "/tmp/gw/gw.sock"

// clean is a contained gate run's full live report (testdata, captured
// from rootless Podman 4.9.3), without its "done", for attempt.
func clean(t *testing.T, attempt string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/probe-gate.txt")
	must(t, err)
	return strings.NewReplacer("@ROOT@", "/tmp/adcp1", "@ATTEMPT@", attempt).Replace(string(b))
}

// The probe's verdict goes red under each F fault the run could suffer:
// a network, a controller path or mount, a controller socket, a HOME with
// content, capabilities, no seccomp, limits not in force.
func TestProbeVerdict(t *testing.T) {
	base := clean(t, "a1")
	gw := "mount " + GatewaySocket + " " + testGateway + " ext4\nsocket " + GatewaySocket + "\n"
	userdata := "/tmp/adcp1/r/overlay-containers/0abc/userdata/"
	cases := []struct {
		name string
		mode Mode
		out  string
		want []string // violations, by prefix; nil is a pass
	}{
		{"gate, contained", Gate, base, nil},
		{"a filesystem mounted at runs/", Gate, base + "mount /workspace /a1/workspace ext4\nmount /results /a1/results ext4\n", nil},
		{"another attempt under a mount", Gate, base + "mount /workspace /a2/workspace ext4\n", []string{"a mount from the host: /workspace"}},
		{"build, gateway only", Build, base + gw, nil},
		{"a network interface", Gate, base + "iface eth0\n", []string{"network interface eth0"}},
		{"a route", Gate, base + "routes 2\n", []string{"a network route"}},
		{"the state directory", Gate, base + "present /srv/adc/state\n", []string{"controller path present: /srv/adc/state"}},
		{"workspace from the state dir", Gate, base + "mount /workspace /srv/adc/state ext4\n", []string{"a mount from the host: /workspace"}},
		{"results from another attempt", Gate, base + "mount /results /tmp/adcp1/runs/a2/results ext4\n", []string{"a mount from the host: /results"}},
		{"gateway from another socket", Build, base + "mount " + GatewaySocket + " /run/adc/adcd.sock ext4\nsocket " + GatewaySocket + "\n", []string{"a mount from the host: " + GatewaySocket}},
		{"a host bind elsewhere", Gate, base + "mount /mnt/x /srv/adc/state ext4\n", []string{"a mount from the host: /mnt/x"}},
		{"a host filesystem whole", Gate, base + "mount /mnt/x / ext4\n", []string{"a mount from the host: /mnt/x"}},
		{"a tmpfs at an unpinned point", Gate, base + "mount /run / tmpfs\n", []string{"a mount from the host: /run"}},
		{"a pinned point of another type", Gate, base + "mount /tmp / ext4\n", []string{"a mount from the host: /tmp"}},
		{"a device node elsewhere", Gate, base + "mount /mnt/null /null devtmpfs\n", []string{"a mount from the host: /mnt/null"}},
		{"a disk device", Gate, base + "mount /dev/sda /sda devtmpfs\n", []string{"a mount from the host: /dev/sda"}},
		{"proc at another point", Gate, base + "mount /mnt/p /sys proc\n", []string{"a mount from the host: /mnt/p"}},
		{"a bind posing as Podman's", Gate, base + "mount /etc/hosts /home/op/.ssh/known_hosts ext4\n", []string{"a mount from the host: /etc/hosts"}},
		{"Podman's file at the wrong point", Gate, base + "mount /etc/hosts " + userdata + "hostname ext4\n", []string{"a mount from the host: /etc/hosts"}},
		{"HOME has content", Gate, base + "home .netrc\n", []string{"HOME is not empty"}},
		{"effective capabilities", Gate, base + "status CapEff 000001ffffffffff\n", []string{"CapEff is 000001ffffffffff"}},
		{"permitted capabilities", Gate, base + "status CapPrm 0000000000000400\n", []string{"CapPrm is"}},
		{"a bounding set", Gate, base + "status CapBnd 000001ffffffffff\n", []string{"CapBnd is"}},
		{"ambient capabilities", Gate, base + "status CapAmb 0000000000000400\n", []string{"CapAmb is"}},
		{"no seccomp", Gate, base + "status Seccomp 0\n", []string{"Seccomp is 0"}},
		{"no_new_privs off", Gate, base + "status NoNewPrivs 0\n", []string{"NoNewPrivs is 0"}},
		{"no pids limit", Gate, base + "limit pids max\n", []string{"the pids limit is max"}},
		{"no memory limit", Gate, base + "limit memory max\n", []string{"the memory limit is max"}},
		{"no cpu limit", Gate, base + "limit cpu max/100000\n", []string{"the cpu limit is max/100000"}},
		{"gate with the gateway mounted", Gate, base + gw, []string{"a mount from the host: " + GatewaySocket, "socket reachable"}},
		{"build with adcd's socket too", Build, base + gw + "socket /mnt/adcd.sock\n", []string{"socket reachable: /mnt/adcd.sock"}},
		{"build without the gateway", Build, base, []string{"the gateway socket is absent"}},
	}
	for _, c := range cases {
		r, err := parse([]byte(c.out + "done\n"))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		r.Mode, r.attempt, r.gateway = c.mode, "a1", testGateway
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
// that did not report its status or limits, nor its run's own mounts.
func TestProbeIncomplete(t *testing.T) {
	base := clean(t, "a1")
	for _, out := range []string{"", base, "unreadable\n" + base + "done\n", "routes x\ndone\n",
		base + "surprise\ndone\n", base + "4747 2821 0:287 / / ro\ndone\n"} {
		if _, err := parse([]byte(out)); !errors.Is(err, ErrProbe) {
			t.Errorf("%q: %v, want ErrProbe", out, err)
		}
	}
	r, err := parse([]byte("iface lo\nroutes 0\ndone\n"))
	if err != nil || len(r.check()) == 0 {
		t.Errorf("a report without status or limits passed: %v", err)
	}
	r, err = parse([]byte(base + "done\n"))
	if r.Mode = Gate; err != nil || len(r.check()) == 0 {
		t.Errorf("a report checked without its attempt passed: %v", err)
	}
}

// Probe runs the fixed script in a fresh attempt, looks for every
// forbidden path and the controller's home, judges the mounts by that
// attempt, and tears the attempt down.
func TestProbeRuns(t *testing.T) {
	f := &fake{fn: func(_ context.Context, args []string) ([]byte, error) {
		for _, a := range args {
			if src, ok := strings.CutPrefix(a, "--mount=type=bind,src="); ok && strings.HasSuffix(a, ",dst="+WorkspaceDir) {
				return []byte(clean(t, filepath.Base(filepath.Dir(strings.TrimSuffix(src, ",dst="+WorkspaceDir)))) + "done\n"), nil
			}
		}
		return nil, nil // the removal after the run
	}}
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
	if len(f.calls) != 2 || !slices.Contains(f.calls[1], "rm") {
		t.Errorf("calls = %q, want the run then the removal of its container", f.calls)
	}
	if runs, _ := os.ReadDir(h.Root + "/runs"); len(runs) != 0 {
		t.Errorf("the probe's attempt was not torn down: %v", runs)
	}
}
