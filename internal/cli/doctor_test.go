// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// doctorEnv is a separated host: the daemon runs as another uid whose
// home is a temp directory, and nothing it holds can be opened.
func doctorEnv(t *testing.T) (*doctorClient, system, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // never the user's real ~/.config/adc
	daemonHome := t.TempDir()
	sys := system{
		uid:  func() int { return 1000 },
		home: func(uid int) (string, error) { return daemonHome, nil },
		open: func(string, bool) error { return unix.EACCES },
	}
	return &doctorClient{reader: reader{body: whoamiBody("supervisor")}, daemon: 2000, seen: true}, sys, daemonHome
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

// A properly separated supervisor gets no warning; the doctor still names
// what it cannot see.
func TestDoctorSeparated(t *testing.T) {
	c, sys, _ := doctorEnv(t)
	code, rep, errb := runDoctor(t, c, sys, "--state-dir", "/srv/adc/state")
	if code != entry.ExitOK || len(rep.Warnings) != 0 || checks(rep.Passed) != "daemon_uid,state_dir,operator_token" ||
		checks(rep.Unseen) != checkElsewhere || c.resource != "whoami" {
		t.Fatalf("%d %+v %s", code, rep, errb)
	}
}

// Each D31 condition warns with exit 0 and names itself.
func TestDoctorWarnings(t *testing.T) {
	t.Run("daemon uid", func(t *testing.T) {
		c, sys, _ := doctorEnv(t)
		c.daemon = 1000
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
		if err := os.MkdirAll(filepath.Join(home, ".config", "adc"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".config", "adc", "token"), []byte(secret+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, rep, _ := runDoctor(t, c, sys, "--state-dir", filepath.Join(home, "absent"))
		if code != entry.ExitOK || checks(rep.Warnings) != checkOperatorToken {
			t.Fatalf("%d %+v", code, rep)
		}
		var out bytes.Buffer
		doctor(context.Background(), nil, &out, &out, c, sys)
		if !strings.Contains(out.String(), "warning: operator_token: this user can open the operator token file") ||
			strings.Contains(out.String(), "SECRETVALUE") {
			t.Fatalf("human: %q", out.String())
		}
	})
}

// An operator token gets no supervisor check, even as the daemon uid.
func TestDoctorOperator(t *testing.T) {
	c, sys, _ := doctorEnv(t)
	c.body, c.daemon = whoamiBody("operator"), 1000
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
	code, rep, _ := runDoctor(t, c, sys)
	if code != entry.ExitOK || len(rep.Warnings)+len(rep.Passed) != 0 || checks(rep.Unseen) != "daemon_uid,state_dir,operator_token_elsewhere" {
		t.Fatalf("%d %+v", code, rep)
	}
	var out bytes.Buffer
	doctor(context.Background(), []string{"--state-dir", "/s\x1b[2J"}, &out, &out, c, sys)
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), `cannot see: state_dir: the state directory /s\u{001B}[2J could not be checked`) {
		t.Fatalf("human: %q", out.String())
	}
}

// The socket transport proves its peer's uid; HTTPS proves none.
func TestDoctorTransportPeer(t *testing.T) {
	if uid, ok := (client.Transport{Socket: "s", DaemonUID: 7}).DaemonPeer(); uid != 7 || !ok {
		t.Errorf("socket: %d %v", uid, ok)
	}
	if _, ok := (client.Transport{Remote: &client.Remote{}}).DaemonPeer(); ok {
		t.Error("https: a peer uid")
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
	c.err, c.body = nil, `{"kind":"root"}`
	if code, _, errb := runDoctor(t, c, sys); code != entry.ExitCouldNotRun || strings.Contains(errb, "root") {
		t.Errorf("invalid: %d %q", code, errb)
	}
}
