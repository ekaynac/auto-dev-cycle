// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
)

// jailProbeName is this test binary's name when it plays a jailed adapter
// (TestTheJailedAdapterReachesItsGateway): it dials ADC_GATEWAY_SOCKET,
// says "hello" and exits.
const jailProbeName = "adc-jail-probe"

func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == jailProbeName {
		c, err := net.Dial("unix", os.Getenv("ADC_GATEWAY_SOCKET"))
		if err != nil {
			os.Exit(3)
		}
		_, _ = c.Write([]byte("hello\n"))
		c.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Opus 22a-5c-2a M1: adcd's host adapter is jailed: it writes only the
// attempt's workspace, exchange and socket, reads only the system's
// directories and its own, and its environment is its PATH and its socket,
// never the controller's.
func TestTheHostAdapterIsJailed(t *testing.T) {
	h := hostAdapter{Command: []string{"/opt/adc/http-chat/adapter"}, Env: adapterEnv}
	run := AdapterRun{Attempt: "att_01m3250v9xft08ssr4ygq6g2np", Workspace: "/srv/sbx/runs/a1/workspace", Exchange: "/srv/sbx/runs/exchange/exchange-att_01m3250v9xft08ssr4ygq6g2np",
		Socket: "/srv/sbx/runs/gateway/att_01m3250v9xft08ssr4ygq6g2np.sock", Deadline: time.Minute}
	s, err := h.Start(run)
	if err != nil {
		t.Fatal(err)
	}
	j := s.Config.Jail
	if j == nil || !slices.Equal(j.Writable, []string{run.Workspace, run.Exchange, run.Socket}) ||
		!slices.Equal(j.ReadOnly, append(slices.Clone(systemDirs), "/opt/adc/http-chat")) {
		t.Fatalf("the jail: %+v", j)
	}
	if !slices.Equal(s.Config.Env, []string{"PATH=/usr/local/bin:/usr/bin:/bin", "ADC_GATEWAY_SOCKET=" + run.Socket}) {
		t.Errorf("the environment: %v", s.Config.Env)
	}
}

// Opus 22a-5c-2a-w L1: through hostAdapter.Start and session.Open, as the
// runner starts it, a jailed adapter reaches its gateway's socket, bound
// into the jail, with the environment hostAdapter gives it (its PATH and
// its socket). The adapter is this test binary, copied apart from /tmp,
// which the jail mounts afresh.
func TestTheJailedAdapterReachesItsGateway(t *testing.T) {
	base, err := os.MkdirTemp(cmpOr(os.Getenv("ADC_TEST_WORK"), "/var/tmp"), "jail-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	bin, ws, exch := filepath.Join(base, "bin"), filepath.Join(base, "workspace"), filepath.Join(base, "exchange")
	for _, d := range []string{bin, ws, exch} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	var b []byte
	if err == nil {
		b, err = os.ReadFile(self)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(bin, jailProbeName), b, 0o700)
	}
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(base, "gw.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	said := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		line := make([]byte, 6)
		n, _ := c.Read(line)
		said <- string(line[:n])
	}()
	sess, err := hostAdapter{Command: []string{filepath.Join(bin, jailProbeName)}, Env: adapterEnv}.Start(AdapterRun{Attempt: "att_01m3250v9xft08ssr4ygq6g2np",
		Workspace: ws, Exchange: exch, Socket: sock, Deadline: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	s, err := session.Open(sess.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	select {
	case got := <-said:
		if got != "hello\n" {
			t.Errorf("the gateway heard %q", got)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the jailed adapter never reached its gateway's socket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case <-s.Done():
	case <-ctx.Done():
	}
}

// cmpOr is a, or b when a is empty.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
