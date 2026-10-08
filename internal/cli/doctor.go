// SPDX-License-Identifier: Apache-2.0

package cli

// adc doctor (04 "Enforcement statement (D31)", 15 D3): supervisor limits
// hold only if the supervisor's OS user cannot read an operator token or
// the controller's state. For a supervisor token it warns when the CLI
// runs as the daemon uid, can open the state directory, or can open the
// daemon user's operator token file; it never reads a file's content, and
// it names what it cannot see. A warning is advice: the exit code is 0.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

// The doctor's checks, by the name its findings carry.
const (
	checkDaemonUID     = "daemon_uid"
	checkStateDir      = "state_dir"
	checkOperatorToken = "operator_token"
	checkElsewhere     = "operator_token_elsewhere"
)

// peer is a Client whose transport knows the uid its peer was checked to
// run as (client.Transport over the socket).
type peer interface {
	DaemonPeer() (uid int, ok bool)
}

var _ peer = client.Transport{}

// system is what the doctor learns from the host; tests replace it.
type system struct {
	uid  func() int
	home func(uid int) (string, error)
	// open opens path read-only, without following a symlink, and closes
	// it at once: nothing is read.
	open func(path string, dir bool) error
}

func hostSystem() system {
	return system{
		uid: os.Getuid,
		home: func(uid int) (string, error) {
			u, err := user.LookupId(strconv.Itoa(uid))
			if err != nil {
				return "", err
			}
			return u.HomeDir, nil
		},
		open: func(path string, dir bool) error {
			flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_NOCTTY | unix.O_CLOEXEC
			if dir {
				flags |= unix.O_DIRECTORY
			}
			fd, err := unix.Open(path, flags, 0)
			if err != nil {
				return err
			}
			return unix.Close(fd)
		},
	}
}

type finding struct {
	Check  string `json:"check"`
	Detail string `json:"detail"`
}

type doctorReport struct {
	Actor    string    `json:"actor"`
	Kind     string    `json:"kind"`
	Listener string    `json:"listener"`
	Warnings []finding `json:"warnings"`
	Passed   []finding `json:"passed"`
	Unseen   []finding `json:"unseen"`
}

func doctor(ctx context.Context, args []string, stdout, stderr io.Writer, c Client, sys system) int {
	const name = "adc doctor"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "the controller's state directory, as adcd run is given it")
	asJSON := fs.Bool("json", false, "print the report as one JSON document")
	if code, ok := parse(fs, args, stderr); !ok {
		return code
	}
	if fs.NArg() > 0 {
		say(stderr, "%s: unexpected argument %s\n", name, fs.Arg(0))
		return entry.ExitUsage
	}
	r, ok := c.(Reader)
	if !ok {
		return failed(stderr, name, ErrNoTransport)
	}
	raw, err := r.Read(ctx, client.Whoami)
	if err != nil {
		return failed(stderr, name, err)
	}
	var who types.Whoami
	if err := json.Unmarshal(raw, &who); err != nil || (who.Kind != types.WhoamiKindOperator && who.Kind != types.WhoamiKindSupervisor) {
		say(stderr, "%s: could_not_run: the controller's whoami is not valid\n", name)
		return entry.ExitCouldNotRun
	}
	rep := doctorReport{Actor: string(who.Actor), Kind: string(who.Kind), Listener: string(who.Listener),
		Warnings: []finding{}, Passed: []finding{}, Unseen: []finding{}}
	if who.Kind == types.WhoamiKindSupervisor {
		rep = examine(rep, c, sys, *stateDir)
	}
	if *asJSON {
		doc, _ := json.Marshal(rep) // strings only: it cannot fail
		err = sayJSON(stdout, doc)
	} else {
		err = describeDoctor(stdout, rep)
	}
	if err != nil {
		say(stderr, "%s: could_not_run: writing the report: %s\n", name, err)
		return entry.ExitCouldNotRun
	}
	return entry.ExitOK
}

// examine runs the supervisor checks of D31 and returns the report with
// their findings.
func examine(rep doctorReport, c Client, sys system, stateDir string) doctorReport {
	add := func(list *[]finding, check, detail string) { *list = append(*list, finding{check, detail}) }
	uid := sys.uid()
	daemon, seen := 0, false
	if p, ok := c.(peer); ok {
		daemon, seen = p.DaemonPeer()
	}
	switch {
	case !seen:
		add(&rep.Unseen, checkDaemonUID, "this transport shows no peer credentials, so the daemon's uid and its operator token file are unknown")
	case daemon == uid:
		add(&rep.Warnings, checkDaemonUID, "this supervisor token is used as the daemon's uid "+strconv.Itoa(uid)+", which can read the operator token and the controller's state")
	default:
		add(&rep.Passed, checkDaemonUID, "the daemon runs as uid "+strconv.Itoa(daemon)+", not this uid "+strconv.Itoa(uid))
	}
	if stateDir == "" {
		add(&rep.Unseen, checkStateDir, "no --state-dir given: the daemon exposes no path, so the state directory was not checked")
	} else {
		reach(&rep, checkStateDir, "the state directory "+stateDir, sys.open(stateDir, true))
	}
	if seen {
		if home, err := sys.home(daemon); err != nil || home == "" {
			add(&rep.Unseen, checkOperatorToken, "the daemon uid "+strconv.Itoa(daemon)+" has no home directory this user can look up, so its operator token file was not checked")
		} else {
			reach(&rep, checkOperatorToken, "the operator token file "+filepath.Join(home, ".config", "adc", "token"),
				sys.open(filepath.Join(home, ".config", "adc", "token"), false))
		}
	}
	add(&rep.Unseen, checkElsewhere, "an operator token held anywhere else, which this user may still be able to read")
	return rep
}

// reach records what opening what returned: opened is a warning, a
// permission refusal or no such file passes, and anything else is unseen.
func reach(rep *doctorReport, check, what string, err error) {
	switch {
	case err == nil:
		rep.Warnings = append(rep.Warnings, finding{check, "this user can open " + what})
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		rep.Passed = append(rep.Passed, finding{check, "this user cannot open " + what})
	case errors.Is(err, unix.ENOENT):
		rep.Passed = append(rep.Passed, finding{check, "there is no " + what})
	default:
		rep.Unseen = append(rep.Unseen, finding{check, what + " could not be checked: " + err.Error()})
	}
}

// describeDoctor writes the report's human lines, every value through say.
func describeDoctor(w io.Writer, rep doctorReport) error {
	if err := say(w, "doctor: %s token of %s over %s\n", rep.Kind, rep.Actor, rep.Listener); err != nil {
		return err
	}
	if rep.Kind != string(types.WhoamiKindSupervisor) {
		return say(w, "  the supervisor isolation checks do not apply to an operator token\n")
	}
	for _, g := range []struct {
		label string
		list  []finding
	}{{"warning", rep.Warnings}, {"ok", rep.Passed}, {"cannot see", rep.Unseen}} {
		for _, f := range g.list {
			if err := say(w, "  %s: %s: %s\n", g.label, f.Check, f.Detail); err != nil {
				return err
			}
		}
	}
	return nil
}
