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
	"os"
	"os/exec"
	"path/filepath"
	"testing"

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
	names, err := h.listed(context.Background(), "^"+ContainerName(att)+"$")
	must(t, err)
	return names
}

// The interactive run is P9's containment: the fixed probe, run through the
// session, finds no violation, its exchange mounted from its own root, and
// the gateway socket the only socket.
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
// then ends; in/ is read-only and out/ is the host's; the container is gone
// once Wait returns.
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
		if left := l.listedNow(t, l.h, att); len(left) != 1 { // the control: the crash leaves it
			t.Fatalf("after the client's death: %q", left)
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
