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
	// open checks path's type with lstat (a directory when dir, else a
	// regular file: never a symlink, FIFO or device), then opens it
	// read-only and closes it at once: nothing is read.
	open func(path string, dir bool) error
}

// wrongType: the path is not of the type probed, so it was not opened.
type wrongType string

func (w wrongType) Error() string { return "is " + string(w) }

var typeNames = map[uint32]wrongType{unix.S_IFLNK: "a symlink", unix.S_IFIFO: "a FIFO", unix.S_IFSOCK: "a socket",
	unix.S_IFCHR: "a device", unix.S_IFBLK: "a device", unix.S_IFDIR: "a directory", unix.S_IFREG: "a regular file"}

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
			var st unix.Stat_t
			if err := unix.Lstat(path, &st); err != nil {
				return err
			}
			want, flags := uint32(unix.S_IFREG), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC
			if dir {
				want, flags = unix.S_IFDIR, flags|unix.O_DIRECTORY
			}
			if t := st.Mode & unix.S_IFMT; t != want {
				return typeNames[t]
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
	if err := json.Unmarshal(raw, &who); err != nil || who.Actor == "" ||
		(who.Kind != types.WhoamiKindOperator && who.Kind != types.WhoamiKindSupervisor) {
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
		add(&rep.Unseen, checkDaemonUID, "this transport shows no peer credentials, so the daemon's uid is unknown")
	case daemon == uid:
		add(&rep.Warnings, checkDaemonUID, "this supervisor token is used as the daemon's uid "+strconv.Itoa(uid)+", which can read the operator token and the controller's state")
	default:
		add(&rep.Passed, checkDaemonUID, "the daemon runs as uid "+strconv.Itoa(daemon)+", not this uid "+strconv.Itoa(uid))
	}
	if stateDir == "" {
		add(&rep.Unseen, checkStateDir, "no --state-dir given: the daemon exposes no path, so the state directory was not checked")
	} else {
		// A refusal passes only because adcd starts on nothing but a 0700
		// directory of its own (internal/daemon/startup.go:31), so this
		// user cannot hold search alone on it.
		reach(&rep, checkStateDir, "the state directory "+stateDir, "adcd refuses symlinked or non-directory state",
			" (adcd keeps it 0700)", "the supplied state directory "+stateDir+" does not exist", sys.open(stateDir, true))
	}
	home, err := "", error(nil)
	if seen {
		home, err = sys.home(daemon)
	}
	notSeen := "; a token kept elsewhere, or under a HOME other than the daemon user's passwd home, is not seen"
	switch {
	case !seen:
		add(&rep.Unseen, checkOperatorToken, "the daemon's uid is unknown over this transport, so its token file was not checked")
	case err != nil || !filepath.IsAbs(home):
		add(&rep.Unseen, checkOperatorToken, "the daemon uid "+strconv.Itoa(daemon)+" has no passwd home this user can look up, so its token file was not checked"+notSeen)
	default:
		path := filepath.Join(home, ".config", "adc", "token")
		what := "the daemon user's token file " + path + ", where the operator token the private socket accepts is kept"
		if daemon == uid {
			what = "its own token file " + path + " (this uid is the daemon's)"
		}
		reach(&rep, checkOperatorToken, what, "the CLI refuses a token that is not a regular file", "",
			"no token file at "+path+notSeen, sys.open(path, false))
	}
	add(&rep.Unseen, checkElsewhere, "an operator token held anywhere else, which this user may still be able to read")
	return rep
}

// reach records what probing what returned: opened is a warning; a
// permission refusal passes (with why it can); a missing path, a wrong
// type or any other error is unseen, never a pass.
func reach(rep *doctorReport, check, what, refuses, why, missing string, err error) {
	var wt wrongType
	switch {
	case err == nil:
		rep.Warnings = append(rep.Warnings, finding{check, "this user can open " + what})
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		rep.Passed = append(rep.Passed, finding{check, "this user cannot open " + what + why})
	case errors.Is(err, unix.ENOENT):
		rep.Unseen = append(rep.Unseen, finding{check, missing})
	case errors.As(err, &wt):
		rep.Unseen = append(rep.Unseen, finding{check, what + " " + wt.Error() + "; " + refuses + ", so it was not opened"})
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
