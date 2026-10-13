// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// probeScript is the fixed probe (P9): run by /bin/sh inside the sandbox,
// with the paths to look for as its arguments. It reports, one fact per
// line: each network interface and the routing table's entries; HOME's
// entries; each argument present; every mount (point, root within its
// filesystem, type); the process's effective capabilities, no_new_privs
// and seccomp mode; the cgroup's memory and pids limits; and every socket
// outside /proc and /sys. Sockets are found by stat (test -S), never by
// find -type s: a socket bind-mounted over a placeholder file reads as a
// regular file in its directory. "done" proves it ran to its end.
const probeScript = `set -u
command -v find >/dev/null && command -v sed >/dev/null || exit 3
for i in /sys/class/net/*; do [ -e "$i" ] && echo "iface ${i##*/}"; done
[ -r /proc/net/route ] && [ -r /proc/net/ipv6_route ] || { echo unreadable; exit 3; }
n=0; while read -r i r; do [ "$i" = Iface ] || [ "$i" = lo ] || n=$((n+1)); done </proc/net/route
while read -r a b c d e f g h k i; do [ "$i" = lo ] || n=$((n+1)); done </proc/net/ipv6_route
echo "routes $n"
for f in "$HOME"/* "$HOME"/.[!.]* "$HOME"/..?*; do { [ -e "$f" ] || [ -L "$f" ]; } && echo "home ${f##*/}"; done
for p in "$@"; do { [ -e "$p" ] || [ -L "$p" ]; } && echo "present $p"; done
sed -E 's/^[0-9]+ [0-9]+ [0-9:]+ ([^ ]+) ([^ ]+) .* - ([^ ]+) .*$/mount \2 \1 \3/' /proc/self/mountinfo
while IFS=: read -r k v; do case "$k" in CapEff|NoNewPrivs|Seccomp) echo "status $k" $v ;; esac; done </proc/self/status
echo "limit memory $(cat /sys/fs/cgroup/memory.max)"; echo "limit pids $(cat /sys/fs/cgroup/pids.max)"
find / \( -path /proc -o -path /sys \) -prune -o -print 2>/dev/null | while IFS= read -r f; do [ -S "$f" ] && echo "socket $f"; done
echo done
`

// probeLimits are the probe run's limits, which the probe must find in force.
var probeLimits = Limits{Memory: 256 << 20, CPUs: "1", PIDs: 64, Timeout: time.Minute}

// Mount is one mount the probe saw.
type Mount struct {
	Point string `json:"point"`
	Root  string `json:"root"` // the path within its filesystem: "/" unless a bind
	Type  string `json:"type"`
}

// Report is what the probe saw, and what in it breaks the mode's
// containment (Violations); an empty Violations is a pass.
type Report struct {
	Mode       Mode              `json:"mode"`
	Image      string            `json:"image"`
	Interfaces []string          `json:"interfaces"`
	Routes     int               `json:"routes"`
	Home       []string          `json:"home_entries"`
	Present    []string          `json:"forbidden_present"`
	Mounts     []Mount           `json:"mounts"`
	Status     map[string]string `json:"status"`
	Limits     map[string]string `json:"limits"`
	Sockets    []string          `json:"sockets"`
	Violations []string          `json:"violations"`
}

// ErrProbe: the probe did not run to its end; nothing is proven.
var ErrProbe = errors.New("sandbox: the probe did not complete")

// Probe runs the fixed probe in a fresh run of img in mode, with no task
// or lease. gateway is the host socket a build run gets (empty for gate).
// It looks for the forbidden paths and the controller's home inside.
func (h Host) Probe(ctx context.Context, img Image, mode Mode, gateway string) (Report, error) {
	a, err := h.NewAttempt()
	if err != nil {
		return Report{}, err
	}
	argv := append(append([]string{"/bin/sh", "-c", probeScript, "probe"}, h.Forbidden...), h.Home)
	out, err := h.Start(ctx, Run{Image: img, Mode: mode, Attempt: a, Gateway: gateway, Limits: probeLimits, Argv: argv})
	if !a.unsafe {
		err = errors.Join(err, a.Teardown())
	}
	if err != nil {
		return Report{}, err
	}
	r, err := parse(out)
	if err != nil {
		return Report{}, err
	}
	r.Mode, r.Image = mode, img.Pinned
	r.Violations = r.check()
	return r, nil
}

func parse(out []byte) (Report, error) {
	r := Report{Interfaces: []string{}, Home: []string{}, Present: []string{}, Mounts: []Mount{}, Sockets: []string{},
		Status: map[string]string{}, Limits: map[string]string{}}
	done := false
	lists := map[string]*[]string{"iface": &r.Interfaces, "home": &r.Home, "present": &r.Present, "socket": &r.Sockets}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		key, val, _ := strings.Cut(sc.Text(), " ")
		if l, ok := lists[key]; ok {
			*l = append(*l, val)
			continue
		}
		k, err := strconv.Atoi(val)
		f := strings.Fields(val)
		switch {
		case key == "done":
			done = true
		case key == "routes" && err == nil:
			r.Routes += k
		case key == "mount" && len(f) == 3:
			r.Mounts = append(r.Mounts, Mount{Point: f[0], Root: f[1], Type: f[2]})
		case key == "status" && len(f) == 2:
			r.Status[f[0]] = f[1]
		case key == "limit" && len(f) == 2:
			r.Limits[f[0]] = f[1]
		default: // "unreadable" or anything unexpected: nothing is proven
			return Report{}, fmt.Errorf("%w: %s", ErrProbe, key)
		}
	}
	if !done || sc.Err() != nil {
		return Report{}, ErrProbe
	}
	return r, nil
}

// Podman's own file mounts (from its runroot) and the filesystems it
// mounts whole.
var (
	podmanFiles = []string{"/run/.containerenv", "/etc/hostname", "/etc/resolv.conf", "/etc/hosts"}
	whole       = []string{"proc", "sysfs", "tmpfs", "cgroup2", "devpts", "mqueue"}
)

// check is the acceptance (00 §5.1, P9): no network route, an empty HOME,
// no forbidden path, no mount from the host but the run's own, no
// capability, no_new_privs and seccomp filtering, the limits in force,
// and no socket but the gateway's, which a build run must have.
func (r Report) check() []string {
	v := []string{}
	for _, i := range r.Interfaces {
		if i != "lo" {
			v = append(v, "network interface "+i)
		}
	}
	if r.Routes > 0 {
		v = append(v, "a network route")
	}
	if len(r.Home) > 0 {
		v = append(v, "HOME is not empty")
	}
	for _, p := range r.Present {
		v = append(v, "controller path present: "+p)
	}
	own := []string{WorkspaceDir, ResultsDir}
	if r.Mode == Build {
		own = append(own, GatewaySocket)
	}
	for _, m := range r.Mounts {
		pseudo := (m.Type == "proc" || m.Type == "devtmpfs") && (strings.HasPrefix(m.Point, "/proc/") || strings.HasPrefix(m.Point, "/dev/"))
		ok := slices.Contains(own, m.Point) || pseudo ||
			slices.Contains(podmanFiles, m.Point) && strings.Contains(m.Root, "/userdata/") ||
			m.Root == "/" && (slices.Contains(whole, m.Type) || m.Type == "overlay" && m.Point == "/")
		if !ok {
			v = append(v, "a mount from the host: "+m.Point+" ("+m.Type+" "+m.Root+")")
		}
	}
	for k, want := range map[string]string{"CapEff": "0000000000000000", "NoNewPrivs": "1", "Seccomp": "2"} {
		if r.Status[k] != want {
			v = append(v, k+" is "+r.Status[k]+", want "+want)
		}
	}
	for k, want := range map[string]string{"memory": strconv.FormatInt(probeLimits.Memory, 10), "pids": strconv.Itoa(probeLimits.PIDs)} {
		if r.Limits[k] != want {
			v = append(v, "the "+k+" limit is "+r.Limits[k]+", want "+want)
		}
	}
	for _, s := range r.Sockets {
		if r.Mode != Build || s != GatewaySocket {
			v = append(v, "socket reachable: "+s)
		}
	}
	if r.Mode == Build && !slices.Contains(r.Sockets, GatewaySocket) {
		v = append(v, "the gateway socket is absent")
	}
	slices.Sort(v)
	return v
}
