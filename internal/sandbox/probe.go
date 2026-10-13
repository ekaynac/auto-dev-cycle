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
// line: each network interface and the routing table's entries, HOME's
// entries, each argument present, and every socket outside /proc and /sys.
// Sockets are found by stat (test -S), never by find -type s: a socket
// bind-mounted over a placeholder file reads as a regular file in its
// directory. Bind mounts are walked (no -xdev). "done" proves it ran to
// its end.
const probeScript = `set -u
command -v find >/dev/null || exit 3
for i in /sys/class/net/*; do [ -e "$i" ] && echo "iface ${i##*/}"; done
[ -r /proc/net/route ] && [ -r /proc/net/ipv6_route ] || { echo unreadable; exit 3; }
n=0; while read -r i r; do [ "$i" = Iface ] || [ "$i" = lo ] || n=$((n+1)); done </proc/net/route
while read -r a b c d e f g h k i; do [ "$i" = lo ] || n=$((n+1)); done </proc/net/ipv6_route
echo "routes $n"
for f in "$HOME"/* "$HOME"/.[!.]* "$HOME"/..?*; do { [ -e "$f" ] || [ -L "$f" ]; } && echo "home ${f##*/}"; done
for p in "$@"; do { [ -e "$p" ] || [ -L "$p" ]; } && echo "present $p"; done
find / \( -path /proc -o -path /sys \) -prune -o -print 2>/dev/null | while IFS= read -r f; do [ -S "$f" ] && echo "socket $f"; done
echo done
`

// Report is what the probe saw, and what in it breaks the mode's
// containment (Violations); an empty Violations is a pass.
type Report struct {
	Mode       Mode     `json:"mode"`
	Image      string   `json:"image"`
	Interfaces []string `json:"interfaces"`
	Routes     int      `json:"routes"`
	Home       []string `json:"home_entries"`
	Present    []string `json:"forbidden_present"`
	Sockets    []string `json:"sockets"`
	Violations []string `json:"violations"`
}

// ErrProbe: the probe did not run to its end; nothing is proven.
var ErrProbe = errors.New("sandbox: the probe did not complete")

// Probe runs the fixed probe in a fresh run of img in mode, with no task
// or lease. gateway is the host socket a build run gets (empty for gate).
// It looks for the forbidden paths and the controller's home inside.
func (h Host) Probe(ctx context.Context, img Image, mode Mode, gateway, home string) (Report, error) {
	a, err := h.NewAttempt()
	if err != nil {
		return Report{}, err
	}
	argv := append([]string{"/bin/sh", "-c", probeScript, "probe"}, h.Forbidden...)
	if home != "" {
		argv = append(argv, home)
	}
	out, err := h.Start(ctx, Run{Image: img, Mode: mode, Workspace: a.Workspace(), Results: a.Results(), Gateway: gateway,
		Limits: Limits{Memory: "256m", CPUs: "1", PIDs: 64, Timeout: time.Minute}, Argv: argv})
	if terr := a.Teardown(); err == nil {
		err = terr
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
	r := Report{Interfaces: []string{}, Home: []string{}, Present: []string{}, Sockets: []string{}}
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
		switch {
		case key == "done":
			done = true
		case key == "routes" && err == nil:
			r.Routes += k
		default: // "unreadable" or anything unexpected: nothing is proven
			return Report{}, fmt.Errorf("%w: %s", ErrProbe, key)
		}
	}
	if !done || sc.Err() != nil {
		return Report{}, ErrProbe
	}
	return r, nil
}

// check is the acceptance (00 §5.1, P9): no network route, an empty
// HOME, no forbidden path, and no socket but the gateway's, which a
// build run must have.
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
	for _, s := range r.Sockets {
		if r.Mode != Build || s != GatewaySocket {
			v = append(v, "socket reachable: "+s)
		}
	}
	if r.Mode == Build && !slices.Contains(r.Sockets, GatewaySocket) {
		v = append(v, "the gateway socket is absent")
	}
	return v
}
