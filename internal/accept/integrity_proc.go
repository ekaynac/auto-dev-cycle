// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
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
// child reads blobs and lists imports through the controller: blobs of
// this run only, within the run's read, request and listing budgets; go
// list runs in the controller's Lister (SandboxLister: the prepared Go
// image). A child that cannot start, ends without its result or runs past
// the deadline blocks: unverifiable. The guard's code in the child is
// trusted (it has the controller's uid and file system: the process is for
// its lifecycle, not a confinement); what the child sends is bounded before
// the controller allocates it, and a failure the controller saw is never
// overridden by the child's result.

var maxRunTime = 10 * time.Minute // one guard run, from its start; type checks end by maxListTime

// The child's messages are frames bounded before allocation (Astra #287
// H1): a length (at most maxFrame, all of a run's within maxProcOut), an
// op, then its fields, each length and count checked against its bound and
// what is left of the frame.
var (
	maxProcOut    int64 = 256 << 20
	maxFrame            = 16 << 20
	maxMsgEntries       = 1 << 16 // blocks, warnings or paths in one message
)

// procStarted is a test's: a run's child, once started.
var procStarted func(*os.Process)

const childArg, childEnv = "adc-accept-integrity-guard", "ADC_ACCEPT_INTEGRITY_GUARD"

// RunChildIfAsked runs this process as Integrity's child, and exits, when
// the controller started it as one (its argument, and its environment
// naming its parent); otherwise it returns at once. A binary that runs
// Integrity calls it first in main (os.Executable() is the child's
// binary), as this package's TestMain does: never in an init, which would
// run the guard before a later file's init.
func RunChildIfAsked() {
	if len(os.Args) != 2 || os.Args[1] != childArg || os.Getenv(childEnv) == "" {
		return
	} else if os.Getenv(childEnv) != strconv.Itoa(os.Getppid()) { // its controller died before Pdeathsig held
		os.Exit(2)
	}
	os.Exit(child(os.Stdin, os.Stdout))
}

// procMsg is the controller's message to the child: the run, then each answer.
type procMsg struct {
	Changes         []Change
	Files           map[string]string
	Over            bool // the base's file list is over its bound
	ListEnd         time.Time
	Dir             string
	Lister          bool
	B               []byte
	OK              bool
	Listed          map[string]Listed
	Err, Panic      string
	Erred, Panicked bool // an error or a panic, whatever its text
}

// childMsg is one of the child's: a read ('r'), a list ('l') or the result ('d').
type childMsg struct {
	op               byte
	blob             string
	max              int64
	platform         [2]string
	gomod, gosum     []byte
	paths            []string
	blocks, warnings []Block
	err              string
	erred            bool
}

func (m childMsg) frame() []byte {
	b := []byte{0, 0, 0, 0, m.op}
	num := func(n int) { b = binary.AppendUvarint(b, uint64(n)) }
	str := func(s string) { num(len(s)); b = append(b, s...) }
	switch m.op {
	case 'r':
		str(m.blob)
		num(int(m.max))
	case 'l':
		str(m.platform[0])
		str(m.platform[1])
		str(string(m.gomod))
		str(string(m.gosum))
		num(len(m.paths))
		for _, p := range m.paths {
			str(p)
		}
	case 'd':
		for _, bs := range [][]Block{m.blocks, m.warnings} {
			num(len(bs))
			for _, x := range bs {
				str(x.Reason.String())
				str(x.Path)
				str(x.What)
				b = append(b, byte(b2i(x.Additive)|b2i(x.Typed)<<1))
			}
		}
		str(m.err)
		b = append(b, byte(b2i(m.erred)))
	}
	binary.BigEndian.PutUint32(b, uint32(len(b)-4))
	return b
}

// readFrame reads one child message, its size, counts and lengths checked
// before anything is allocated for them; left is what the run may still read.
func readFrame(r io.Reader, left *int64) (childMsg, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return childMsg{}, err
	}
	size := int64(binary.BigEndian.Uint32(n[:]))
	if *left -= 4 + size; size < 1 || size > int64(maxFrame) || *left < 0 {
		return childMsg{}, errors.New("a message over its bound")
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return childMsg{}, err
	}
	m, p, bad := childMsg{op: b[0]}, b[1:], false
	num := func(max, per int) int { // at most max, and per bytes each in what is left
		v, k := binary.Uvarint(p)
		if bad = bad || k <= 0 || v > uint64(max) || per > 0 && v > uint64((len(p)-k)/per); bad {
			return 0
		}
		p = p[k:]
		return int(v)
	}
	str := func(max int) string {
		n := num(max, 0)
		if bad = bad || n > len(p); bad {
			return ""
		}
		s := string(p[:n])
		p = p[n:]
		return s
	}
	flag := func() byte {
		if bad = bad || len(p) == 0; bad {
			return 0
		}
		f := p[0]
		p = p[1:]
		return f
	}
	switch m.op {
	case 'r':
		m.blob, m.max = str(64), int64(num(int(maxFile), 0))
	case 'l':
		m.platform, m.gomod, m.gosum = [2]string{str(16), str(16)}, []byte(str(int(maxFile))), []byte(str(int(maxFile)))
		m.paths = make([]string, num(maxMsgEntries, 1))
		for i := range m.paths {
			m.paths[i] = str(4096)
		}
	case 'd':
		for _, to := range []*[]Block{&m.blocks, &m.warnings} {
			*to = make([]Block, num(maxMsgEntries, 4))
			for i := range *to {
				reason, path, what, f := str(64), str(4096), str(1<<16), flag()
				bad = bad || reason != vocab.ReasonTestIntegrityViolation.String() || f > 3 // the guard's only reason
				(*to)[i] = Block{vocab.ReasonTestIntegrityViolation, path, what, f&1 != 0, f&2 != 0}
			}
		}
		m.err, m.erred = str(4096), flag() == 1
	default:
		bad = true
	}
	if bad || len(p) != 0 {
		return childMsg{}, errors.New("a malformed message")
	}
	return m, nil
}

// Integrity is the integrity guard for a candidate of base in repo, for
// Accept's In.Guard: the base's files listed here through a temporary
// index, the guard in a child process reading blobs through read and
// imports' types through list (nil: every Go package unverifiable), all by
// one deadline. The run's own directory, what list copies out, is made
// under scratch: the sandbox root's runs/ for SandboxLister (a filesystem
// of its own); "" is the system's temporary directory.
func Integrity(repo sandbox.Repo, base string, list Lister, scratch string) Guard {
	return func(ctx context.Context, changes []Change, read Reader) ([]Block, []Block, error) {
		runCtx, cancel := context.WithTimeout(ctx, maxRunTime)
		defer cancel()
		blocks, warnings, err := isolated(runCtx, repo, base, changes, read, list, scratch)
		switch {
		case ctx.Err() != nil:
			return nil, nil, ctx.Err()
		case runCtx.Err() != nil: // a result past it, too
			return unverifiable("the guard did not finish by its whole-run deadline"), nil, nil
		}
		return blocks, warnings, err
	}
}

func unverifiable(why string) []Block {
	return []Block{{vocab.ReasonTestIntegrityViolation, "", "unverifiable: " + why, false, false}}
}

// isolated runs the guard in a child until ctx ends, serving its reads and
// lists; a stuck service never holds up the child's kill, its reaping or
// the directory's removal.
func isolated(ctx context.Context, repo sandbox.Repo, base string, changes []Change, read Reader, list Lister, scratch string) ([]Block, []Block, error) {
	files, err := listBase(ctx, repo, base)
	if err != nil {
		return nil, nil, err
	} else if scratch != "" {
		if err := os.MkdirAll(scratch, 0o700); err != nil {
			return nil, nil, err
		}
	}
	dir, err := os.MkdirTemp(scratch, "adc-guard-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir) // after the child ends: it reads what list copies here
	exe, err := os.Executable()
	if err != nil {
		return unverifiable("the guard's process did not start"), nil, nil
	}
	cmd, stderr := exec.CommandContext(ctx, exe, childArg), &head{n: 4096}
	cmd.Env, cmd.Dir, cmd.Stderr, cmd.WaitDelay = []string{childEnv + "=" + strconv.Itoa(os.Getpid())}, "/", stderr, time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} // a controller that dies takes it along
	in, err := cmd.StdinPipe()
	out, err2 := cmd.StdoutPipe()
	if err = errors.Join(err, err2); err == nil {
		err = cmd.Start()
	}
	if err != nil {
		return unverifiable("the guard's process did not start"), nil, nil
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }() // its abandoned type checks with it
	// The kernel's first choice when memory runs out: the child, not the controller.
	_ = os.WriteFile("/proc/"+strconv.Itoa(cmd.Process.Pid)+"/oom_score_adj", []byte("1000"), 0)
	if procStarted != nil {
		procStarted(cmd.Process)
	}
	req := procMsg{Changes: changes, Files: files, Over: files == nil, ListEnd: time.Now().Add(maxListTime), Dir: dir, Lister: list != nil}
	blocks, warnings, err, ok := converse(ctx, in, out, req, read, list)
	if !ok {
		_ = cmd.Process.Kill()
		_ = cmd.Wait() // its stderr is whole once Wait returns
		return unverifiable(fmt.Sprintf("the guard's process ended without its result (%s)", bytes.TrimSpace(stderr.b))), nil, nil
	}
	return blocks, warnings, err
}

// head keeps the first n bytes written (a crash's reason comes first).
type head struct {
	b []byte
	n int
}

func (h *head) Write(p []byte) (int, error) {
	h.b = append(h.b, p[:min(len(p), max(h.n-len(h.b), 0))]...)
	return len(p), nil
}

// service is what the controller holds of a child's run: the blobs it may
// read, its budgets, and the failures the controller saw (latched: the
// child's result never overrides them, Astra #287 M4).
type service struct {
	req             procMsg
	allowed         map[string]bool
	read            Reader
	list            Lister
	readLeft        int64
	requests, lists int
	readErr         error
	failed          string // a block the result must hold
}

// converse serves the child on in and out until its result (ok), ctx's end
// or a message refused.
func converse(ctx context.Context, in io.Writer, out io.Reader, req procMsg, read Reader, list Lister) ([]Block, []Block, error, bool) {
	s := &service{req: req, allowed: map[string]bool{}, read: read, list: list, readLeft: maxRead + maxCgoRead}
	for _, b := range req.Files {
		s.allowed[b] = true
	}
	for _, c := range req.Changes {
		s.allowed[c.SrcBlob], s.allowed[c.DstBlob] = true, true
	}
	enc, left := gob.NewEncoder(in), maxProcOut
	for m := req; enc.Encode(m) == nil; {
		c, err := readFrame(out, &left)
		if err != nil {
			return nil, nil, nil, false
		} else if c.op == 'd' {
			switch {
			case s.readErr != nil: // the reader's error is the guard's, whatever the child says
				return c.blocks, c.warnings, s.readErr, true
			case s.failed != "" && !slices.ContainsFunc(c.blocks, func(b Block) bool { return b.What == s.failed }):
				return append(c.blocks, Block{vocab.ReasonTestIntegrityViolation, "", s.failed, false, false}), c.warnings, nil, true
			case c.erred:
				return c.blocks, c.warnings, errors.New(c.err), true
			}
			return c.blocks, c.warnings, nil, true
		}
		served := make(chan procMsg, 1)
		go func() { served <- s.serve(ctx, c) }()
		select {
		case m = <-served:
		case <-ctx.Done(): // a service stuck past the deadline is left behind
			return nil, nil, nil, false
		}
	}
	return nil, nil, nil, false
}

// serve answers a child's read or list; a panic is passed back, the guard's
// fault as in this process, and latched. A request outside the run's blobs
// or budgets is refused as one.
func (s *service) serve(ctx context.Context, c childMsg) (m procMsg) {
	defer func() {
		if p := recover(); p != nil {
			s.failed = cmp.Or(s.failed, fmt.Sprintf("unverifiable: the guard failed (%v)", p))
			m = procMsg{Panic: fmt.Sprint(p), Panicked: true}
		}
	}()
	if s.requests++; s.requests > 64*len(s.allowed)+maxListRuns+1024 {
		panic("the guard's process past its request bound")
	}
	switch {
	case c.op == 'r' && !s.allowed[c.blob]:
		panic("the guard's process asked for a blob outside its run")
	case c.op == 'r':
		b, ok, err := s.read(ctx, c.blob, max(min(c.max, maxFile, s.readLeft), 0))
		s.readLeft -= int64(len(b))
		s.readErr = cmp.Or(s.readErr, err)
		return procMsg{B: b, OK: ok, Err: errText(err), Erred: err != nil}
	case s.lists >= maxListRuns || s.list == nil:
		panic("the guard's process past its go list bound")
	}
	s.lists++
	lctx, cancel := context.WithDeadline(ctx, s.req.ListEnd)
	defer cancel()
	l, err := s.list(lctx, s.req.Dir, c.platform, c.gomod, c.gosum, c.paths)
	return procMsg{Listed: l, Err: errText(err), Erred: err != nil}
}

// child runs the guard in this process for the controller on in and out.
func child(in io.Reader, out io.Writer) int {
	dec := gob.NewDecoder(in)
	var req procMsg
	if dec.Decode(&req) != nil {
		return 2
	}
	call := func(m childMsg) procMsg {
		var rep procMsg
		if _, err := out.Write(m.frame()); err != nil || dec.Decode(&rep) != nil {
			os.Exit(2) // the controller is gone
		} else if rep.Panicked {
			panic(rep.Panic)
		}
		return rep
	}
	errOf := func(rep procMsg) error {
		if !rep.Erred {
			return nil
		}
		return errors.New(rep.Err)
	}
	read := func(_ context.Context, blob string, max int64) ([]byte, bool, error) {
		rep := call(childMsg{op: 'r', blob: blob, max: max})
		return rep.B, rep.OK, errOf(rep)
	}
	var list Lister
	if req.Lister {
		list = func(_ context.Context, _ string, pl [2]string, gomod, gosum []byte, paths []string) (map[string]Listed, error) {
			rep := call(childMsg{op: 'l', platform: pl, gomod: gomod, gosum: gosum, paths: paths})
			return rep.Listed, errOf(rep)
		}
	}
	if req.Files == nil && !req.Over { // gob sends no empty map
		req.Files = map[string]string{}
	}
	blocks, warnings, err := guard(context.Background(), req.Changes, read, list, req.Files, req.ListEnd, req.Dir)
	if _, err := out.Write(childMsg{op: 'd', blocks: blocks, warnings: warnings, err: errText(err)[:min(len(errText(err)), 4096)], erred: err != nil}.frame()); err != nil {
		return 2
	}
	return 0
}
