// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// doctorClient answers whoami with body; with seen, its peer was checked
// to run as daemon.
type doctorClient struct {
	reader
	daemon int
	seen   bool
}

func (d *doctorClient) DaemonPeer() (int, bool) { return d.daemon, d.seen }

func whoamiBody(kind string) string {
	return `{"actor":"act_1","kind":"` + kind + `","token":"tok_1","listener":"shared_socket","reads":{"everything":false,"grants":[]}}`
}

// doctorEnv is a separated host: this user is uid 4242, the daemon runs
// as 4343, whose home (and only its) is a temp directory, and nothing it
// holds can be opened.
func doctorEnv(t *testing.T) (*doctorClient, system, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // never the user's real ~/.config/adc
	daemonHome := t.TempDir()
	c := &doctorClient{reader: reader{body: whoamiBody("supervisor")}, daemon: 4343, seen: true}
	sys := system{
		uid: func() int { return 4242 },
		home: func(uid int) (string, error) {
			if uid != c.daemon {
				return "", errors.New("not the daemon uid")
			}
			return daemonHome, nil
		},
		open: func(string, bool) error { return unix.EACCES },
	}
	return c, sys, daemonHome
}

func runDoctor(t *testing.T, c Client, sys system, args ...string) (int, doctorReport, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := doctor(context.Background(), append(args, "--json"), &out, &errb, c, sys)
	var rep doctorReport
	if code == entry.ExitOK {
		if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
			t.Fatalf("report: %v %q", err, out.String())
		}
	}
	return code, rep, errb.String()
}

func checks(fs []finding) string {
	var names []string
	for _, f := range fs {
		names = append(names, f.Check)
	}
	return strings.Join(names, ",")
}

// detail is the one finding of check in fs, or "".
func detail(fs []finding, check string) string {
	for _, f := range fs {
		if f.Check == check {
			return f.Detail
		}
	}
	return ""
}

// A properly separated supervisor gets no warning (EACCES or EPERM); the
// doctor still names what it cannot see.
func TestDoctorSeparated(t *testing.T) {
	for _, errno := range []error{unix.EACCES, unix.EPERM} {
		c, sys, _ := doctorEnv(t)
		sys.open = func(string, bool) error { return errno }
		code, rep, errb := runDoctor(t, c, sys, "--state-dir", "/srv/adc/state")
		if code != entry.ExitOK || len(rep.Warnings) != 0 || checks(rep.Passed) != "daemon_uid,state_dir,operator_token" ||
			checks(rep.Unseen) != checkElsewhere || c.resource != "whoami" || !strings.Contains(detail(rep.Passed, checkStateDir), "0700") {
			t.Fatalf("%v: %d %+v %s", errno, code, rep, errb)
		}
	}
}

// Each D31 condition warns with exit 0 and names itself.
func TestDoctorWarnings(t *testing.T) {
	t.Run("daemon uid", func(t *testing.T) {
		c, sys, _ := doctorEnv(t)
		c.daemon = 4242
		if code, rep, _ := runDoctor(t, c, sys); code != entry.ExitOK || checks(rep.Warnings) != checkDaemonUID {
			t.Fatalf("%d %+v", code, rep)
		}
	})
	t.Run("state directory", func(t *testing.T) {
		c, sys, _ := doctorEnv(t)
		sys.open = hostSystem().open
		if code, rep, _ := runDoctor(t, c, sys, "--state-dir", t.TempDir()); code != entry.ExitOK || checks(rep.Warnings) != checkStateDir {
			t.Fatalf("%d %+v", code, rep)
		}
	})
	t.Run("operator token", func(t *testing.T) {
		c, sys, home := doctorEnv(t)
		sys.open = hostSystem().open
		secret := "tok_SECRETVALUE.not-a-real-secret"
		plantToken(t, home, secret)
		code, rep, _ := runDoctor(t, c, sys, "--state-dir", filepath.Join(home, "absent"))
		if code != entry.ExitOK || checks(rep.Warnings) != checkOperatorToken ||
			!strings.Contains(detail(rep.Warnings, checkOperatorToken), "the daemon user's token file") ||
			detail(rep.Unseen, checkStateDir) != "the supplied state directory "+filepath.Join(home, "absent")+" does not exist" {
			t.Fatalf("%d %+v", code, rep)
		}
		var out bytes.Buffer
		doctor(context.Background(), nil, &out, &out, c, sys)
		if !strings.Contains(out.String(), "warning: operator_token: this user can open the daemon user's token file") ||
			strings.Contains(out.String(), "SECRETVALUE") {
			t.Fatalf("human: %q", out.String())
		}
		// As the daemon's uid, the file is its own.
		c.daemon = 4242
		sys.uid = func() int { return 4242 }
		if _, rep, _ := runDoctor(t, c, sys); !strings.Contains(detail(rep.Warnings, checkOperatorToken), "its own token file") {
			t.Fatalf("same uid: %+v", rep)
		}
	})
}

func plantToken(t *testing.T, home, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".config", "adc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "adc", "token"), []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Never a false "ok": a missing path, a wrong type, or no usable home is
// unseen. A FIFO is never opened (the probe returns at once).
func TestDoctorProbe(t *testing.T) {
	cases := map[string]struct {
		setup              func(t *testing.T, home, state string)
		stateWant, tokWant string
	}{
		"missing": {func(*testing.T, string, string) {}, "does not exist", "no token file at"},
		"fifo token": {func(t *testing.T, home, state string) {
			plantToken(t, home, "x")
			os.Remove(filepath.Join(home, ".config", "adc", "token"))
			if err := unix.Mkfifo(filepath.Join(home, ".config", "adc", "token"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "", "is a FIFO; the CLI refuses"},
		"symlinks": {func(t *testing.T, home, state string) {
			plantToken(t, home, "x")
			tok := filepath.Join(home, ".config", "adc", "token")
			os.Rename(tok, tok+".real")
			os.Symlink(tok+".real", tok)
			os.Symlink(t.TempDir(), state)
		}, "is a symlink; adcd refuses symlinked", "is a symlink; the CLI refuses"},
		"wrong kinds": {func(t *testing.T, home, state string) {
			os.MkdirAll(filepath.Join(home, ".config", "adc", "token"), 0o700)
			os.WriteFile(state, nil, 0o600)
		}, "is a regular file; adcd refuses", "is a directory; the CLI refuses"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, sys, home := doctorEnv(t)
			sys.open = hostSystem().open
			state := filepath.Join(t.TempDir(), "state")
			tc.setup(t, home, state)
			done := make(chan doctorReport, 1)
			go func() { _, rep, _ := runDoctor(t, c, sys, "--state-dir", state); done <- rep }()
			var rep doctorReport
			select {
			case rep = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the probe blocked")
			}
			st, tok := detail(rep.Unseen, checkStateDir), detail(rep.Unseen, checkOperatorToken)
			if len(rep.Warnings)+len(rep.Passed) != 1 || !strings.Contains(st, tc.stateWant) || st == "" ||
				!strings.Contains(tok, tc.tokWant) || !strings.Contains(tok, "a HOME other than") && name == "missing" {
				t.Fatalf("%+v", rep)
			}
		})
	}
	for _, home := range []string{"", "relative/home"} {
		c, sys, _ := doctorEnv(t)
		sys.home = func(int) (string, error) { return home, nil }
		sys.open = func(string, bool) error { return nil }
		if _, rep, _ := runDoctor(t, c, sys); !strings.Contains(detail(rep.Unseen, checkOperatorToken), "no passwd home") {
			t.Fatalf("home %q: %+v", home, rep)
		}
	}
}

// An operator token gets no supervisor check, even as the daemon uid.
func TestDoctorOperator(t *testing.T) {
	c, sys, _ := doctorEnv(t)
	c.body, c.daemon = whoamiBody("operator"), 4242
	sys.open = func(string, bool) error { return nil }
	code, rep, _ := runDoctor(t, c, sys, "--state-dir", "/srv/adc/state")
	if code != entry.ExitOK || len(rep.Warnings)+len(rep.Passed)+len(rep.Unseen) != 0 {
		t.Fatalf("%d %+v", code, rep)
	}
	// Routed by Main.
	if code, out, _ := run2(c, "doctor"); code != entry.ExitOK || !strings.Contains(out, "do not apply to an operator token") {
		t.Fatalf("main: %d %q", code, out)
	}
}

// Over HTTPS no peer is seen: the uid and the operator token are unseen,
// never passed; a path is escaped in human output.
func TestDoctorUnseen(t *testing.T) {
	c, sys, _ := doctorEnv(t)
	c.seen = false
	sys.open = func(string, bool) error { return unix.EIO }
	sys.home = func(int) (string, error) { t.Error("a home looked up for an unknown uid"); return "", nil }
	code, rep, _ := runDoctor(t, c, sys)
	if code != entry.ExitOK || len(rep.Warnings)+len(rep.Passed) != 0 ||
		checks(rep.Unseen) != "daemon_uid,state_dir,operator_token,operator_token_elsewhere" {
		t.Fatalf("%d %+v", code, rep)
	}
	var out bytes.Buffer
	doctor(context.Background(), []string{"--state-dir", "/s\x1b[2J"}, &out, &out, c, sys)
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), `cannot see: state_dir: the state directory /s\u{001B}[2J could not be checked`) {
		t.Fatalf("human: %q", out.String())
	}
}

// The socket transport proves its peer's uid; HTTPS, or no socket, none.
func TestDoctorTransportPeer(t *testing.T) {
	if uid, ok := (client.Transport{Socket: "s", DaemonUID: 7}).DaemonPeer(); uid != 7 || !ok {
		t.Errorf("socket: %d %v", uid, ok)
	}
	for _, tr := range []client.Transport{{Remote: &client.Remote{}}, {}} {
		if _, ok := tr.DaemonPeer(); ok {
			t.Errorf("%+v: a peer uid", tr)
		}
	}
}

// Failures keep the exit-code table.
func TestDoctorFailures(t *testing.T) {
	c, sys, _ := doctorEnv(t)
	if code, _, _ := runDoctor(t, c, sys, "extra"); code != entry.ExitUsage {
		t.Errorf("argument: %d", code)
	}
	if code, _, _ := runDoctor(t, &fake{}, sys); code != entry.ExitCouldNotRun {
		t.Errorf("no reader: %d", code)
	}
	c.err = refusal(vocab.Failed, vocab.ReasonActionNotGranted)
	if code, _, _ := runDoctor(t, c, sys); code != entry.ExitRefused {
		t.Errorf("refused: %d", code)
	}
	c.err = nil
	for _, body := range []string{`{"actor":"act_1","kind":"root"}`, `{"actor":"","kind":"supervisor"}`, `[`} {
		c.body = body
		if code, _, errb := runDoctor(t, c, sys); code != entry.ExitCouldNotRun || strings.Contains(errb, "root") {
			t.Errorf("invalid %s: %d %q", body, code, errb)
		}
	}
}
