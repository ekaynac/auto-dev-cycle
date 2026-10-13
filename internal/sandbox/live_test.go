// SPDX-License-Identifier: Apache-2.0

package sandbox

// The live tests run the real probe under rootless Podman. They need
// Podman with user namespaces, which the test sandbox (bubblewrap) does
// not offer, so they run only when asked, outside it:
//
//	ADC_LIVE_PODMAN_ARCHIVE=/path/image.oci.tar go test -run Live ./internal/sandbox/
//
// The archive is an OCI archive of any image with /bin/sh, find and sed
// (podman save --format oci-archive -o x.tar debian:bookworm). Each test
// loads it into a fresh, private storage: nothing of the user's Podman is
// read or written, and nothing is pulled over the network.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type live struct {
	h       Host
	img     Image
	gateway string // a listening socket, for build runs
	state   string // a stand-in state directory, forbidden
	adcd    string // a stand-in adcd socket inside it, forbidden
}

func newLive(t *testing.T) live {
	t.Helper()
	archive := os.Getenv("ADC_LIVE_PODMAN_ARCHIVE")
	if archive == "" {
		t.Skip("set ADC_LIVE_PODMAN_ARCHIVE to run the live Podman tests")
	}
	root, err := os.MkdirTemp("", "adcp") // short: Podman's runroot limit
	must(t, err)
	ctl := t.TempDir()
	l := live{state: filepath.Join(ctl, "state"), gateway: filepath.Join(ctl, "gw.sock")}
	l.adcd = filepath.Join(l.state, "adcd.sock")
	must(t, os.Mkdir(l.state, 0o700))
	for _, s := range []string{l.gateway, l.adcd} {
		ln, err := net.Listen("unix", s)
		must(t, err)
		t.Cleanup(func() { ln.Close() })
	}
	l.h = Host{Podman: "/usr/bin/podman", Root: root, Forbidden: []string{l.state, l.adcd}}
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = l.h.podman(ctx, "system", "reset", "--force")
		_ = os.RemoveAll(root)
	})
	if _, err := l.h.podman(ctx, "pull", "--quiet", "oci-archive:"+archive); err != nil {
		t.Fatalf("loading the archive: %v", err)
	}
	l.img = l.pin(t, "")
	return l
}

// pin resolves the only image named like name (any image when empty) by
// its digest, as a profile pins it.
func (l live) pin(t *testing.T, name string) Image {
	t.Helper()
	out, err := l.h.podman(context.Background(), "images", "--format", "json")
	must(t, err)
	var imgs []struct {
		Names       []string
		RepoDigests []string
	}
	must(t, json.Unmarshal(out, &imgs))
	for _, i := range imgs {
		if len(i.RepoDigests) > 0 && (name == "" || slices.ContainsFunc(i.Names, func(n string) bool { return strings.Contains(n, name) })) {
			img, err := l.h.Resolve(context.Background(), i.RepoDigests[0])
			must(t, err)
			return img
		}
	}
	t.Fatalf("no image %q with a digest", name)
	return Image{}
}

func (l live) probe(t *testing.T, mode Mode, mutate func([]string) []string) Report {
	t.Helper()
	h := l.h
	h.mutate = mutate
	gw := ""
	if mode == Build {
		gw = l.gateway
	}
	r, err := h.Probe(context.Background(), l.img, mode, gw, os.Getenv("HOME"))
	must(t, err)
	return r
}

// The acceptance: in both modes, no network, no HOME contents, no state
// directory, no controller socket; the gateway socket in build mode only.
func TestLiveProbeContained(t *testing.T) {
	l := newLive(t)
	for _, mode := range []Mode{Gate, Build} {
		r := l.probe(t, mode, nil)
		if len(r.Violations) != 0 {
			t.Errorf("%s: %+v", mode, r)
		}
		if got := slices.Contains(r.Sockets, GatewaySocket); got != (mode == Build) {
			t.Errorf("%s: gateway socket present = %v", mode, got)
		}
		t.Logf("%s: %+v", mode, r)
	}
}

// The F column, live: each fault injected into the real run (a network,
// the state directory, a controller socket) turns the probe red.
func TestLiveProbeDetectsFaults(t *testing.T) {
	l := newLive(t)
	add := func(extra string) func([]string) []string {
		return func(args []string) []string { return append(args, extra) }
	}
	faults := map[string]struct {
		mutate func([]string) []string
		want   string
	}{
		"network": {func(args []string) []string {
			args[slices.Index(args, "--network=none")] = "--network=slirp4netns"
			return args
		}, "network interface"},
		"state directory":   {add("--mount=type=bind,src=" + l.state + ",dst=" + l.state), "controller path present: " + l.state},
		"controller socket": {add("--mount=type=bind,src=" + l.adcd + ",dst=/mnt/adcd.sock"), "socket reachable: /mnt/adcd.sock"},
		"a populated HOME":  {add("--env=HOME=/etc"), "HOME is not empty"},
	}
	for name, f := range faults {
		for _, mode := range []Mode{Gate, Build} {
			r := l.probe(t, mode, f.mutate)
			if !slices.ContainsFunc(r.Violations, func(v string) bool { return strings.HasPrefix(v, f.want) }) {
				t.Errorf("%s (%s): violations %q, want %q", name, mode, r.Violations, f.want)
			}
		}
	}
}

// An image whose digest differs from the pin is refused; a prepared
// image's manifest is checked against the toolchain and the lockfiles.
func TestLiveImagePin(t *testing.T) {
	l := newLive(t)
	ctx := context.Background()
	repo, _, _ := strings.Cut(l.img.Pinned, "@")
	if _, err := l.h.Resolve(ctx, repo+"@sha256:"+strings.Repeat("0", 64)); !errors.Is(err, ErrImage) {
		t.Fatalf("another digest: %v, want ErrImage", err)
	}
	base := t.TempDir()
	must(t, os.WriteFile(filepath.Join(base, "go.sum"), []byte("sum\n"), 0o600))
	sum, err := fileSum(base, "go.sum")
	must(t, err)
	label, _ := json.Marshal(Manifest{Toolchain: "go", Lockfiles: map[string]string{"go.sum": sum}})
	ctxDir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(ctxDir, "Containerfile"), []byte("FROM "+l.img.ID+"\nLABEL "+ManifestLabel+"='"+string(label)+"'\n"), 0o600))
	archive := filepath.Join(t.TempDir(), "prepared.tar")
	for _, args := range [][]string{
		{"build", "--quiet", "--pull=never", "--network=none", "--tag", "localhost/prepared:1", ctxDir},
		{"push", "--quiet", "localhost/prepared:1", "oci-archive:" + archive + ":localhost/prepared:1"},
		{"rmi", "localhost/prepared:1"},
		{"pull", "--quiet", "oci-archive:" + archive},
	} {
		if _, err := l.h.podman(ctx, args...); err != nil {
			t.Fatalf("%q: %v", args, err)
		}
	}
	prepared := l.pin(t, "prepared")
	if err := prepared.CheckManifest("go", base, []string{"go.sum"}); err != nil {
		t.Fatalf("the prepared image: %v", err)
	}
	must(t, os.WriteFile(filepath.Join(base, "go.sum"), []byte("moved\n"), 0o600))
	if err := prepared.CheckManifest("go", base, []string{"go.sum"}); !errors.Is(err, ErrManifest) {
		t.Fatalf("a lockfile changed at the base: %v, want ErrManifest", err)
	}
	if err := l.img.CheckManifest("go", base, []string{"go.sum"}); !errors.Is(err, ErrManifest) {
		t.Fatalf("an unprepared image: %v, want ErrManifest", err)
	}
}
