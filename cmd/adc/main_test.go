// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/cli"
	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

// The test binary runs as adc itself when asked to.
func TestMain(m *testing.M) {
	if os.Getenv("ADC_MAIN_UNDER_TEST") == "1" {
		os.Exit(run())
	}
	os.Exit(m.Run())
}

// An interrupt while an enrolment waits on the controller cancels it, so
// the reserved --token-out, which no token reached, is removed and adc
// exits could_not_run.
func TestInterruptRemovesTheReservedTokenOut(t *testing.T) {
	dir := testguard.Home(t) // the child's HOME too; short, as a socket path is bounded
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := l.Accept(); err == nil {
			accepted <- c // held open, never answered
		}
	}()
	id, err := ids.New("tok")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "agent.token")
	cmd := exec.Command(os.Args[0], "admin", "enrol-agent", "--expect", "0", "--reason", "r", "--token-out", out,
		"--scope", `{"actor":"act_01m3250vkwffc979nzjw0g2sa1","name":"a","declared_agent":null}`)
	cmd.Env = append(os.Environ(), "ADC_MAIN_UNDER_TEST=1", cli.SocketEnv+"="+sock,
		client.TokenEnv+"="+id+"."+strings.Repeat("A", 43))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-accepted:
		defer c.Close()
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("adc never connected")
	}
	if _, err := os.Lstat(out); err != nil {
		t.Fatalf("--token-out was not reserved before sending: %v", err)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	var exit *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exit) || exit.ExitCode() != entry.ExitCouldNotRun {
		t.Fatalf("exit: %v (%s)", err, stderr.String())
	}
	if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the reserved --token-out was left behind: %v", err)
	}
}
