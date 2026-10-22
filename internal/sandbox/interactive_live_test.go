// SPDX-License-Identifier: Apache-2.0

package sandbox

// P22a-5d's live rows: an adapter's interactive run under rootless Podman,
// through the session's own container start (session.StartContainer). Like
// the other live tests they run only when asked, outside the test sandbox:
//
//	ADC_LIVE_PODMAN_ARCHIVE=/path/image.oci.tar go test -count=1 -run LiveInteractive ./internal/sandbox/
//
// The archive needs /bin/sh, cat, find, sed and sleep (a prepared Go image
// has them). No row dials anything: the run has no network, and the
// gateway is the test's own socket.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
)

// interactive is an interactive build run of a fresh attempt with argv.
func (l live) interactive(t *testing.T, h Host, argv ...string) (Interactive, Run, string) {
	t.Helper()
	att := newAttemptID(t)
	a, err := h.NewAttempt()
	must(t, err)
	t.Cleanup(func() { _ = a.Teardown() })
	r := Run{Image: l.img, Mode: Build, Attempt: a, Gateway: l.gateway, Exchange: exchange(t, h, att), Limits: probeLimits, Argv: argv}
	it, err := h.Interactive(r, att)
	must(t, err)
	return it, r, att
}

func startSession(t *testing.T, it Interactive) *session.Process {
	t.Helper()
	p, err := session.StartContainer(&session.Container{Command: it.Command, Env: it.Env, Remove: it.Remove}, it.Deadline)
	must(t, err)
	t.Cleanup(func() {
		p.Kill()
		_ = p.Wait()
		p.Stdout().Close()
	})
	return p
}

// listedNow are the containers named after attempt's interactive run.
func (l live) listedNow(t *testing.T, h Host, att string) []string {
	t.Helper()
	names, err := h.listed(context.Background(), "name=^"+ContainerName(att)+"$")
	must(t, err)
	return names
}

// runningNow reports whether Podman says attempt's container is running.
func (l live) runningNow(t *testing.T, att string) bool {
	t.Helper()
	out, err := l.h.podman(context.Background(), "inspect", "--format", "{{.State.Running}}", ContainerName(att))
	must(t, err)
	return strings.TrimSpace(string(out)) == "true"
}

// ready starts a session of argv (which says "ready", then sleeps) and
// returns once the container runs.
func (l live) ready(t *testing.T, h Host, deadline time.Duration) (*session.Process, Run, string) {
	t.Helper()
	it, r, att := l.interactive(t, h, "/bin/sh", "-c", "echo ready; exec sleep 100000")
	if deadline > 0 {
		it.Deadline = deadline
	}
	p := startSession(t, it)
	if line, err := bufio.NewReader(p.Stdout()).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("the run did not start: %q, %v", line, err)
	}
	return p, r, att
}

// The interactive run is P9's containment: the fixed probe, run through the
// session, finds no violation, its exchange mounted from its own root (in/
// read-only), and the gateway socket the only socket.
func TestLiveInteractiveContained(t *testing.T) {
	l := newLive(t)
	argv := append(append([]string{"/bin/sh", "-c", probeScript, "probe"}, l.h.Forbidden...), l.h.Home)
	it, r, _ := l.interactive(t, l.h, argv...)
	p := startSession(t, it)
	p.Stdin().Close()
	out, err := io.ReadAll(p.Stdout())
	must(t, err)
	must(t, p.Wait())
	rep, err := parse(out)
	must(t, err)
	rep.Mode, rep.attempt, rep.gateway, rep.exchange = Build, filepath.Base(r.Attempt.Dir), l.gateway, filepath.Base(r.Exchange)
	if v := rep.check(); len(v) != 0 {
		t.Fatalf("violations %q (%+v)", v, rep)
	}
}

// EOF on the session's stdin reaches the process in the container, which
// then ends; in/ is read-only, and what it wrote to out/ is on the host, the
// controller's; the container is gone once Wait returns.
func TestLiveInteractiveEOF(t *testing.T) {
	l := newLive(t)
	it, r, att := l.interactive(t, l.h, "/bin/sh", "-c",
		`touch /exchange/in/x 2>/dev/null && echo in-writable; cat >/exchange/out/got && echo eof-seen`)
	p := startSession(t, it)
	_, err := p.Stdin().Write([]byte("hello\n"))
	must(t, err)
	p.Stdin().Close()
	out, err := io.ReadAll(p.Stdout())
	must(t, err)
	if err := p.Wait(); err != nil || string(out) != "eof-seen\n" {
		t.Fatalf("Wait %v, out %q", err, out)
	}
	if got, err := os.ReadFile(filepath.Join(r.Exchange, "out", "got")); err != nil || string(got) != "hello\n" {
		t.Errorf("out/got: %q, %v", got, err)
	}
	if st, err := os.Lstat(filepath.Join(r.Exchange, "out", "got")); err != nil || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		t.Errorf("out/got is not the controller's: %v", err)
	}
	if left := l.listedNow(t, l.h, att); len(left) != 0 {
		t.Errorf("listed after Wait: %q", left)
	}
}

// A 1 MiB line passes both ways, whole.
func TestLiveInteractiveMegabyteLine(t *testing.T) {
	l := newLive(t)
	it, _, _ := l.interactive(t, l.h, "/bin/cat")
	p := startSession(t, it)
	line := append(bytes.Repeat([]byte("x"), 1<<20), '\n')
	errc := make(chan error, 1)
	go func() {
		_, err := p.Stdin().Write(line)
		errc <- errors.Join(err, p.Stdin().Close())
	}()
	rd := bufio.NewReaderSize(p.Stdout(), 2<<20)
	got, err := rd.ReadBytes('\n')
	must(t, err)
	must(t, <-errc)
	if !bytes.Equal(got, line) {
		t.Fatalf("read %d bytes back, want %d", len(got), len(line))
	}
	if rest, _ := io.ReadAll(rd); len(rest) != 0 {
		t.Errorf("%d bytes after the line", len(rest))
	}
	must(t, p.Wait())
}

// A killed controller (here: its podman client killed, nothing removed)
// leaves its container running under conmon; the start sweep removes it,
// and keeps the one a runner holds.
func TestLiveInteractiveSweptAtStart(t *testing.T) {
	l := newLive(t)
	var atts []string
	for range 2 {
		it, _, att := l.interactive(t, l.h, "/bin/sh", "-c", "echo ready; exec sleep 100000")
		c := exec.Command(it.Command[0], it.Command[1:]...)
		c.Env = it.Env
		in, err := c.StdinPipe()
		must(t, err)
		out, err := c.StdoutPipe()
		must(t, err)
		must(t, c.Start())
		if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
			t.Fatalf("the run did not start: %q, %v", line, err)
		}
		must(t, c.Process.Kill())
		_ = c.Wait()
		in.Close()
		if !l.runningNow(t, att) { // the control: the crash leaves it running
			t.Fatal("the container does not run after its client's death")
		}
		atts = append(atts, att)
	}
	held, stale := atts[0], atts[1]
	removed, err := l.h.RemoveStale(context.Background(), func(a string) bool { return a == held })
	if err != nil || len(removed) != 1 || removed[0] != stale {
		t.Fatalf("removed %q, %v", removed, err)
	}
	if len(l.listedNow(t, l.h, stale)) != 0 || len(l.listedNow(t, l.h, held)) != 1 {
		t.Fatal("the sweep removed the wrong container")
	}
	_, err = l.h.RemoveStale(context.Background(), func(string) bool { return false })
	must(t, err)
}

// Any doubt is ErrUnverified: when Podman cannot remove the container (a
// podman whose rm fails), Wait says so, and the container is indeed still
// there; the attempt stays unsafe.
func TestLiveInteractiveDoubtIsUnverified(t *testing.T) {
	l := newLive(t)
	wrapper := filepath.Join(t.TempDir(), "podman")
	must(t, os.WriteFile(wrapper, []byte("#!/bin/sh\nfor a; do [ \"$a\" = rm ] && exit 1; done\nexec "+l.h.Podman+" \"$@\"\n"), 0o700))
	h := l.h
	h.Podman = wrapper
	it, r, att := l.interactive(t, h, "/bin/sh", "-c", "echo ready; exec sleep 100000")
	p := startSession(t, it)
	if line, err := bufio.NewReader(p.Stdout()).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("the run did not start: %q, %v", line, err)
	}
	p.Kill()
	if err := p.Wait(); !errors.Is(err, session.ErrUnverified) {
		t.Fatalf("Wait %v, want ErrUnverified", err)
	}
	if left := l.listedNow(t, l.h, att); len(left) != 1 {
		t.Fatalf("the container is listed %q: the doubt was not real", left)
	}
	if err := r.Attempt.CopyTree(filepath.Join(t.TempDir(), "tree"), 1<<20); !errors.Is(err, ErrSpec) {
		t.Errorf("the attempt is copied while its container runs: %v", err)
	}
	if _, err := l.h.RemoveStale(context.Background(), func(string) bool { return false }); err != nil || len(l.listedNow(t, l.h, att)) != 0 {
		t.Fatalf("the sweep: %v", err)
	}
}

// The session's deadline removes the container: Wait returns (not
// ErrUnverified), and nothing is listed.
func TestLiveInteractiveDeadline(t *testing.T) {
	l := newLive(t)
	p, _, att := l.ready(t, l.h, 2*time.Second)
	if err := p.Wait(); err == nil || errors.Is(err, session.ErrUnverified) || !p.Expired() {
		t.Fatalf("Wait %v, expired %v", err, p.Expired())
	}
	if left := l.listedNow(t, l.h, att); len(left) != 0 {
		t.Fatalf("listed after the deadline: %q", left)
	}
}

// Kill with a real removal: Wait returns (not ErrUnverified), and nothing
// is listed.
func TestLiveInteractiveKill(t *testing.T) {
	l := newLive(t)
	p, _, att := l.ready(t, l.h, 0)
	p.Kill()
	if err := p.Wait(); err == nil || errors.Is(err, session.ErrUnverified) {
		t.Fatalf("Wait %v", err)
	}
	if left := l.listedNow(t, l.h, att); len(left) != 0 {
		t.Fatalf("listed after Kill: %q", left)
	}
}

// With no controller left (the client killed outright), conmon's own
// --timeout still ends the run: Podman's wait returns, and it runs no more.
func TestLiveInteractiveConmonTimeout(t *testing.T) {
	l := newLive(t)
	att := newAttemptID(t)
	a, err := l.h.NewAttempt()
	must(t, err)
	t.Cleanup(func() { _ = a.Teardown() })
	lim := probeLimits
	lim.Timeout = 2 * time.Second
	it, err := l.h.Interactive(Run{Image: l.img, Mode: Build, Attempt: a, Gateway: l.gateway, Exchange: exchange(t, l.h, att), Limits: lim,
		Argv: []string{"/bin/sh", "-c", "echo ready; exec sleep 100000"}}, att)
	must(t, err)
	c := exec.Command(it.Command[0], it.Command[1:]...)
	c.Env = it.Env
	in, err := c.StdinPipe()
	must(t, err)
	out, err := c.StdoutPipe()
	must(t, err)
	must(t, c.Start())
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("the run did not start: %q, %v", line, err)
	}
	must(t, c.Process.Kill())
	_ = c.Wait()
	in.Close()
	if _, err := l.h.podman(context.Background(), "wait", "--ignore", ContainerName(att)); err != nil {
		t.Fatalf("podman wait: %v", err)
	}
	running, err := l.h.podman(context.Background(), "ps", "--filter=name=^"+ContainerName(att)+"$", "--format={{.Names}}")
	must(t, err)
	if strings.TrimSpace(string(running)) != "" {
		t.Fatalf("still running: %q", running)
	}
	must(t, it.Remove(context.Background()))
}

// A second run of a live attempt id (its client refused: the name is in
// use) never removes the first's container: its Wait is ErrUnverified (the
// name is listed), and the first still runs (#374 M2).
func TestLiveInteractiveSecondRunLeavesTheFirst(t *testing.T) {
	l := newLive(t)
	first, _, att := l.ready(t, l.h, 0)
	a, err := l.h.NewAttempt()
	must(t, err)
	t.Cleanup(func() { _ = a.Teardown() })
	it, err := l.h.Interactive(Run{Image: l.img, Mode: Build, Attempt: a, Gateway: l.gateway, Exchange: filepath.Join(l.h.ExchangeRoot(), "exchange-"+att),
		Limits: probeLimits, Argv: []string{"/bin/true"}}, att)
	must(t, err)
	second := startSession(t, it)
	second.Stdin().Close()
	if err := second.Wait(); !errors.Is(err, session.ErrUnverified) {
		t.Fatalf("the second run's Wait: %v, want ErrUnverified", err)
	}
	if !l.runningNow(t, att) {
		t.Fatal("the second run removed the first's container")
	}
	first.Kill()
	if err := first.Wait(); errors.Is(err, session.ErrUnverified) {
		t.Fatalf("the first run's Wait: %v", err)
	}
}

// No network: a dial to a loopback port the test opened on the host fails
// inside (the container's own network namespace), and the port sees no
// connection; the same dial from the host connects (the control).
func TestLiveInteractiveNoLoopbackToTheHost(t *testing.T) {
	l := newLive(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	t.Cleanup(func() { ln.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	dial := `exec 3<>/dev/tcp/127.0.0.1/` + port + ` && echo connected || echo refused`
	host, err := exec.Command("/bin/bash", "-c", dial).Output()
	if err != nil || string(host) != "connected\n" {
		t.Fatalf("the control (from the host): %q, %v", host, err)
	}
	before := accepted.Load()
	it, _, _ := l.interactive(t, l.h, "/bin/bash", "-c", dial+" 2>/dev/null")
	p := startSession(t, it)
	p.Stdin().Close()
	out, err := io.ReadAll(p.Stdout())
	must(t, err)
	must(t, p.Wait())
	if string(out) != "refused\n" || accepted.Load() != before {
		t.Fatalf("inside: %q; the port saw %d connections from the run", out, accepted.Load()-before)
	}
}

// P22a-5d-3's live row, for a prepared image with the builder's adapter
// (scripts/prepare-go-image.sh; 5d-4's): the http-chat adapter, run as adcd
// runs it (/opt/adc/lib's Node and main.ts, in the attempt's container,
// through the session), answers describe over its stdio, and shuts down
// cleanly; the container is gone after Wait. With
// ADC_LIVE_ADAPTER_RELEASE (a release's lib/adc, e.g. dist/linux-ARCH/lib/adc
// of the image's commit), the image's adapter must be that release's.
//
//	ADC_LIVE_PODMAN_ARCHIVE=/path/prepared.oci.tar ADC_LIVE_ADAPTER=1 go test -count=1 -run LiveInteractiveAdapter ./internal/sandbox/
func TestLiveInteractiveAdapterDescribes(t *testing.T) {
	if os.Getenv("ADC_LIVE_ADAPTER") == "" {
		t.Skip("set ADC_LIVE_ADAPTER with an archive of a prepared image that carries the adapter")
	}
	l := newLive(t)
	if rel := os.Getenv("ADC_LIVE_ADAPTER_RELEASE"); rel != "" {
		want, err := AdapterDigest(rel)
		must(t, err)
		must(t, l.img.CheckAdapter(want))
	}
	it, _, att := l.interactive(t, l.h, "/opt/adc/lib/node/bin/node", "/opt/adc/lib/adapters/http-chat/src/main.ts")
	s, err := session.Open(session.Config{Container: &session.Container{Command: it.Command, Env: it.Env, Remove: it.Remove}, Deadline: it.Deadline})
	must(t, err)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	d, err := s.Describe(ctx)
	if err != nil || d.Harness == "" {
		t.Fatalf("describe: %+v, %v", d, err)
	}
	if o, err := s.Shutdown(ctx); err != nil || o.Outcome != types.OutcomeEmpty {
		t.Fatalf("shutdown: %+v, %v", o, err)
	}
	must(t, s.Wait())
	if left := l.listedNow(t, l.h, att); len(left) != 0 {
		t.Fatalf("listed after Wait: %q", left)
	}
}
