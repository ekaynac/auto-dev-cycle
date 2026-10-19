// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The guard's own process (P14e-2c, operator answer "loader b"): Integrity
// runs the guard in a child of this binary, which the controller kills at
// the whole-run deadline, on a done context, and as soon as it has read
// the result. A type check that never ends (go/types cannot be cancelled)
// ends with the child, and nothing the guard holds outlives its run. The
// child reads blobs and lists imports through the controller (gob messages
// on its stdin and stdout); go list runs in the controller's Lister
// (SandboxLister: the prepared Go image). A child that cannot start, ends
// without its result or runs past the deadline blocks: unverifiable.

var maxRunTime = 10 * time.Minute // one guard run, from its start; type checks end by maxListTime

// maxProcOut bounds what a child writes in one run.
var maxProcOut int64 = 256 << 20

// procStarted is a test's: a run's child, once started.
var procStarted func(*os.Process)

const childArg, childEnv = "adc-accept-integrity-guard", "ADC_ACCEPT_INTEGRITY_GUARD"

func init() {
	if len(os.Args) == 2 && os.Args[1] == childArg && os.Getenv(childEnv) == "1" {
		os.Exit(child(os.Stdin, os.Stdout))
	}
}

// procMsg is one message, either way: the run (the controller's first); a
// read or a list (the child's) and its answer; the result (the child's last).
type procMsg struct {
	Op               string // "read", "list", "done"
	Changes          []Change
	Files            map[string]string
	Over             bool // the base's file list is over its bound
	ListEnd          time.Time
	Dir              string
	Lister           bool
	Blob             string
	Max              int64
	Platform         [2]string
	GoMod, GoSum     []byte
	Paths            []string
	B                []byte
	OK               bool
	Listed           map[string]Listed
	Err, Panic       string
	Blocks, Warnings []wireBlock
}

// wireBlock is a Block in a message (a reason code is text only).
type wireBlock struct {
	Reason, Path, What string
	Additive, Typed    bool
}

func toWire(bs []Block) []wireBlock {
	var out []wireBlock
	for _, b := range bs {
		out = append(out, wireBlock{b.Reason.String(), b.Path, b.What, b.Additive, b.Typed})
	}
	return out
}

func fromWire(ws []wireBlock) ([]Block, error) {
	var out []Block
	for _, w := range ws {
		r, err := vocab.ParseReason(w.Reason)
		if err != nil {
			return nil, err
		}
		out = append(out, Block{r, w.Path, w.What, w.Additive, w.Typed})
	}
	return out, nil
}

// Integrity is the integrity guard for a candidate of base in repo, for
// Accept's In.Guard: the base's files listed here through a temporary
// index, the guard in a child process reading blobs through read and
// imports' types through list (nil: every Go package unverifiable).
func Integrity(repo sandbox.Repo, base string, list Lister) Guard {
	return func(ctx context.Context, changes []Change, read Reader) ([]Block, []Block, error) {
		files, err := listBase(ctx, repo, base)
		if err != nil {
			return nil, nil, err
		}
		dir, err := os.MkdirTemp("", "adc-guard-")
		if err != nil {
			return nil, nil, err
		}
		defer os.RemoveAll(dir) // after the child ends: it reads what list copies here
		start := time.Now()
		return isolated(ctx, start.Add(maxRunTime), procMsg{Changes: changes, Files: files, Over: files == nil,
			ListEnd: start.Add(maxListTime), Dir: dir, Lister: list != nil}, read, list)
	}
}

// isolated runs req's guard in a child until end, serving its reads and lists.
func isolated(ctx context.Context, end time.Time, req procMsg, read Reader, list Lister) ([]Block, []Block, error) {
	unverifiable := func(why string) ([]Block, []Block, error) {
		return []Block{{vocab.ReasonTestIntegrityViolation, "", "unverifiable: " + why, false, false}}, nil, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return unverifiable("the guard's process did not start")
	}
	runCtx, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	cmd := exec.CommandContext(runCtx, exe, childArg)
	cmd.Env, cmd.Dir, cmd.WaitDelay = []string{childEnv + "=1"}, "/", time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} // a controller that dies takes it along
	in, err := cmd.StdinPipe()
	out, err2 := cmd.StdoutPipe()
	if err = errors.Join(err, err2); err == nil {
		err = cmd.Start()
	}
	if err != nil {
		return unverifiable("the guard's process did not start")
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }() // its abandoned type checks with it
	if procStarted != nil {
		procStarted(cmd.Process)
	}
	enc, dec := gob.NewEncoder(in), gob.NewDecoder(io.LimitReader(out, maxProcOut))
	var readErr error // the guard's error is the controller's reader's
	for m := req; enc.Encode(m) == nil; m = serve(ctx, req, m, read, list, &readErr) {
		if m = (procMsg{}); dec.Decode(&m) != nil {
			break
		} else if m.Op != "done" {
			continue
		}
		blocks, err := fromWire(m.Blocks)
		warnings, err2 := fromWire(m.Warnings)
		if err = errors.Join(err, err2); err != nil {
			return unverifiable("the guard's process gave an unknown reason")
		} else if m.Err != "" {
			return blocks, warnings, cmp.Or(readErr, errors.New(m.Err))
		}
		return blocks, warnings, nil
	}
	switch {
	case ctx.Err() != nil:
		return nil, nil, ctx.Err()
	case runCtx.Err() != nil:
		return unverifiable("the guard did not finish by its whole-run deadline")
	}
	return unverifiable("the guard's process ended without its result")
}

// serve answers a child's read or list; a panic is passed back, the guard's
// fault as in this process.
func serve(ctx context.Context, req, got procMsg, read Reader, list Lister, readErr *error) (m procMsg) {
	defer func() {
		if p := recover(); p != nil {
			m = procMsg{Panic: fmt.Sprint(p)}
		}
	}()
	switch {
	case got.Op == "read":
		b, ok, err := read(ctx, got.Blob, got.Max)
		*readErr = cmp.Or(*readErr, err)
		return procMsg{B: b, OK: ok, Err: errText(err)}
	case got.Op == "list" && list != nil:
		lctx, cancel := context.WithDeadline(ctx, req.ListEnd)
		defer cancel()
		l, err := list(lctx, req.Dir, got.Platform, got.GoMod, got.GoSum, got.Paths)
		return procMsg{Listed: l, Err: errText(err)}
	}
	panic("an unknown request from the guard's process")
}

// child runs the guard in this process for the controller on in and out.
func child(in io.Reader, out io.Writer) int {
	dec, enc := gob.NewDecoder(in), gob.NewEncoder(out)
	var req procMsg
	if dec.Decode(&req) != nil {
		return 2
	}
	call := func(m procMsg) procMsg {
		var rep procMsg
		if enc.Encode(m) != nil || dec.Decode(&rep) != nil {
			os.Exit(2) // the controller is gone
		} else if rep.Panic != "" {
			panic(rep.Panic)
		}
		return rep
	}
	errOf := func(s string) error {
		if s == "" {
			return nil
		}
		return errors.New(s)
	}
	read := func(_ context.Context, blob string, max int64) ([]byte, bool, error) {
		rep := call(procMsg{Op: "read", Blob: blob, Max: max})
		return rep.B, rep.OK, errOf(rep.Err)
	}
	var list Lister
	if req.Lister {
		list = func(_ context.Context, _ string, pl [2]string, gomod, gosum []byte, paths []string) (map[string]Listed, error) {
			rep := call(procMsg{Op: "list", Platform: pl, GoMod: gomod, GoSum: gosum, Paths: paths})
			return rep.Listed, errOf(rep.Err)
		}
	}
	if req.Files == nil && !req.Over { // gob sends no empty map
		req.Files = map[string]string{}
	}
	blocks, warnings, err := guard(context.Background(), req.Changes, read, list, req.Files, req.ListEnd, req.Dir)
	if enc.Encode(procMsg{Op: "done", Blocks: toWire(blocks), Warnings: toWire(warnings), Err: errText(err)}) != nil {
		return 2
	}
	return 0
}
