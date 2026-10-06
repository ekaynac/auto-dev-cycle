// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const stubEnv = "ADC_SESSION_STUB"

// stubs are raw adapters the test binary itself plays, written without the
// SDK: ADC_SESSION_STUB names one.
var stubs = map[string]func(){
	// echo copies its input to its output and exits 0 when the input ends.
	"echo": func() { _, _ = io.Copy(os.Stdout, os.Stdin) },
	// tree starts STUB_COUNT (default 1) descendants, each in a session of
	// its own, out of the process group, with no stdio of the adapter's
	// (/dev/null), and STUB_HOGS hogs; says "ready" (unless STUB_QUIET) once
	// all run, and lingers.
	"tree": func() {
		n, err := strconv.Atoi(cmp.Or(os.Getenv("STUB_COUNT"), "1"))
		hogs, err2 := strconv.Atoi(cmp.Or(os.Getenv("STUB_HOGS"), "0"))
		self, err3 := os.Executable()
		if err != nil || err2 != nil || err3 != nil {
			os.Exit(3)
		}
		for range n {
			c := exec.Command("sleep", os.Getenv("STUB_MARKER"))
			c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if c.Start() != nil {
				os.Exit(3)
			}
		}
		for range hogs {
			c := exec.Command(self, os.Getenv("STUB_MARKER"))
			c.Env = []string{stubEnv + "=hog"}
			c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			out, err := c.StdoutPipe()
			if err != nil || c.Start() != nil {
				os.Exit(3)
			}
			if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ok\n" {
				os.Exit(3)
			}
		}
		if os.Getenv("STUB_QUIET") == "" { // a session reads stdout as protocol
			fmt.Println("ready")
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		time.Sleep(time.Hour)
	},
	// hog touches hogBytes of memory, says "ok" and lingers: tearing its
	// memory down at its death takes long enough that the namespace's init
	// is still waiting for it when bubblewrap is already reaped. Without the
	// namespace-exit barrier, Wait returns while it is alive.
	// The memory is mapped and populated by the kernel, outside Go's heap,
	// so a race-instrumented stub is as quick as a plain one.
	"hog": func() {
		b, err := unix.Mmap(-1, 0, hogBytes, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
		if err != nil || unix.Madvise(b, unix.MADV_POPULATE_WRITE) != nil {
			os.Exit(3)
		}
		fmt.Println("ok")
		time.Sleep(time.Hour)
	},
	// secret writes a token it was given to stderr and stdout, then fails.
	"secret": func() {
		fmt.Fprintln(os.Stderr, "token="+os.Getenv("STUB_TOKEN"))
		fmt.Println(`{"token":"` + os.Getenv("STUB_TOKEN") + `"}`)
		os.Exit(3)
	},
	// stderr writes more than the tail, ending with a marker, then exits.
	"stderr": func() {
		w := bufio.NewWriter(os.Stderr)
		w.WriteString(strings.Repeat("x", 3*StderrTail))
		w.WriteString("END")
		w.Flush()
	},
}

// TestMain doubles as the stubs.
func TestMain(m *testing.M) {
	if mode := os.Getenv(stubEnv); mode != "" {
		stubs[mode]()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func stubSpec(t *testing.T, mode string, env ...string) ProcessSpec {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return ProcessSpec{Command: []string{self}, Env: append([]string{"PATH=" + os.Getenv("PATH"), stubEnv + "=" + mode}, env...), Deadline: 30 * time.Second}
}

func spawn(t *testing.T, spec ProcessSpec) *Process {
	t.Helper()
	p, err := Spawn(spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Kill()
		_ = p.Wait()
		p.Stdout().Close()
	})
	return p
}

// waitWithin waits for the process tree within d.
func waitWithin(t *testing.T, p *Process, d time.Duration) error {
	t.Helper()
	select {
	case <-p.Done():
		return p.Wait()
	case <-time.After(d):
		t.Fatalf("the process tree did not exit within %s", d)
		return nil
	}
}

func TestSpawnNeedsACommandAndADeadline(t *testing.T) {
	for _, spec := range []ProcessSpec{{Deadline: time.Second}, {Command: []string{"true"}}} {
		if _, err := Spawn(spec); err == nil {
			t.Errorf("Spawn(%+v) succeeded", spec)
		}
	}
}

func TestStdioAndACleanExit(t *testing.T) {
	p := spawn(t, stubSpec(t, "echo"))
	if _, err := p.Stdin().Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(p.Stdout()).ReadString('\n')
	if err != nil || line != "hello\n" {
		t.Fatalf("read %q, %v", line, err)
	}
	p.Stdin().Close()
	if err := waitWithin(t, p, 10*time.Second); err != nil || p.Expired() {
		t.Fatalf("exit %v, expired %v (stderr %q)", err, p.Expired(), p.Stderr())
	}
}

func TestStderrKeepsOnlyTheTail(t *testing.T) {
	p := spawn(t, stubSpec(t, "stderr"))
	_ = waitWithin(t, p, 10*time.Second)
	got := p.Stderr()
	if len(got) != StderrTail || !strings.HasSuffix(got, "END") {
		t.Fatalf("a %d-byte tail ending %q, want %d bytes ending END", len(got), got[max(0, len(got)-8):], StderrTail)
	}
}

func TestACommandThatCannotRunExits(t *testing.T) {
	spec := stubSpec(t, "echo")
	spec.Command = []string{"/nonexistent/adapter"}
	p := spawn(t, spec)
	if err := waitWithin(t, p, 10*time.Second); err == nil {
		t.Fatal("a command that cannot run exited 0")
	}
}

// running reports whether a process whose argv has the marker exists.
func running(marker string) bool { return len(marked(marker)) > 0 }

// marked lists the pids of processes whose argv has the marker.
func marked(marker string) []int {
	var pids []int
	dirs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, d := range dirs {
		b, err := os.ReadFile(d)
		if err == nil && strings.Contains("\x00"+string(b), "\x00"+marker+"\x00") {
			pid, _ := strconv.Atoi(strings.Split(d, "/")[2])
			pids = append(pids, pid)
		}
	}
	return pids
}

func newMarker() string { return fmt.Sprintf("%d.%03d", 600+rand.IntN(1000), rand.IntN(1000)) }

// tree spawns the tree stub with n descendants, waits for its "ready" and
// returns pidfds on the descendants, held so that their exit can be checked
// at an instant (a pidfd is readable once its process has exited).
func tree(t *testing.T, n int, deadline time.Duration) (*Process, []int) {
	t.Helper()
	marker := newMarker()
	spec := stubSpec(t, "tree", "STUB_MARKER="+marker, "STUB_COUNT="+strconv.Itoa(n), "STUB_HOGS="+strconv.Itoa(treeHogs))
	n += treeHogs
	spec.Deadline = deadline
	p := spawn(t, spec)
	if line, err := bufio.NewReader(p.Stdout()).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("the tree stub said %q, %v", line, err)
	}
	var fds []int
	for _, pid := range marked(marker) {
		if fd, err := unix.PidfdOpen(pid, 0); err == nil {
			fds = append(fds, fd)
			t.Cleanup(func() { unix.Close(fd) })
		}
	}
	if len(fds) != n {
		t.Fatalf("%d of %d descendants seen", len(fds), n)
	}
	return p, fds
}

// survivors counts the pidfds whose process has not exited, now.
func survivors(t *testing.T, fds []int) int {
	t.Helper()
	polls := make([]unix.PollFd, len(fds))
	for i, fd := range fds {
		polls[i] = unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}
	}
	if _, err := unix.Poll(polls, 0); err != nil {
		t.Fatal(err)
	}
	alive := 0
	for _, q := range polls {
		if q.Revents&unix.POLLIN == 0 {
			alive++
		}
	}
	return alive
}

// treeSize is enough descendants that a Wait without the namespace-exit
// barrier returns while some still run (Astra's probe: 33 of 100).
const treeSize = 100

// treeHogs and hogBytes: descendants whose death is slow (see the hog
// stub), so that a Wait without the barrier is caught every time.
const (
	treeHogs = 2
	hogBytes = 512 << 20
)

func TestKillEndsTheWholeTree(t *testing.T) {
	p, fds := tree(t, treeSize, 30*time.Second)
	p.Kill()
	err := waitWithin(t, p, 10*time.Second)
	// Checked at Wait's return, no grace period.
	if n := survivors(t, fds); n != 0 || errors.Is(err, ErrUnverified) {
		t.Fatalf("%d descendants survived Wait (%v)", n, err)
	}
	if p.Expired() {
		t.Fatal("Kill was reported as the deadline")
	}
}

func TestTheDeadlineKillsTheTree(t *testing.T) {
	p, fds := tree(t, treeSize, 5*time.Second)
	err := waitWithin(t, p, 10*time.Second)
	if n := survivors(t, fds); n != 0 || !p.Expired() || errors.Is(err, ErrUnverified) {
		t.Fatalf("%d descendants survived the deadline (expired %v, %v)", n, p.Expired(), err)
	}
}

// TestSpawnWithoutTheInitFails: without a verified pidfd on the namespace's
// init, Wait could not prove the tree gone, so Spawn returns no Process.
func TestSpawnWithoutTheInitFails(t *testing.T) {
	saved := openInit
	t.Cleanup(func() { openInit = saved })
	openInit = func(int, int) int { return -1 }
	if p, err := Spawn(stubSpec(t, "echo")); err == nil || p != nil {
		t.Fatalf("Spawn without the init's pidfd returned %v, %v", p, err)
	}
}

// fakeToken stands for a secret an adapter is given (a gateway token).
const fakeToken = "sk-fake-0123456789abcdef"

func TestNoSecretInErrors(t *testing.T) {
	p := spawn(t, stubSpec(t, "secret", "STUB_TOKEN="+fakeToken))
	err := waitWithin(t, p, 10*time.Second)
	if err == nil {
		t.Fatal("exit 3 was not an error")
	}
	if strings.Contains(err.Error(), fakeToken) {
		t.Fatalf("the exit error leaks the token: %v", err)
	}
	if !strings.Contains(p.Stderr(), fakeToken) {
		t.Fatal("the stderr accessor lost what the adapter wrote")
	}
}

func TestKillAfterWaitSendsNothing(t *testing.T) {
	p := spawn(t, stubSpec(t, "echo"))
	p.mu.Lock()
	opened := p.bwrapFD >= 0 && p.initFD >= 0
	p.mu.Unlock()
	if !opened {
		t.Fatal("no pidfd for bubblewrap or the namespace's init")
	}
	p.Stdin().Close()
	if err := waitWithin(t, p, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	// The pidfds are closed before Wait returns: nothing can be signalled,
	// whatever reuses the pids (or the descriptor numbers) since.
	if n := p.kill(); n != 0 {
		t.Fatalf("kill after Wait sent %d signals", n)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bwrapFD != -1 || p.initFD != -1 {
		t.Fatalf("pidfds still open after Wait: %d %d", p.bwrapFD, p.initFD)
	}
}

// resources counts this process's open descriptors and goroutines.
func resources() (fds, goroutines int) {
	entries, _ := os.ReadDir("/proc/self/fd")
	return len(entries), runtime.NumGoroutine()
}

// noLeak checks, when the test ends, that every descriptor and goroutine
// it started is released (goroutines get a moment to return).
func noLeak(t *testing.T) {
	t.Helper()
	fds0, g0 := resources()
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			fds, g := resources()
			if fds <= fds0 && g <= g0 {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("leaked: %d descriptors (was %d), %d goroutines (was %d)", fds, fds0, g, g0)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

// TestADeadlineDuringStartupKillsTheInit: the deadline fires after the
// init's pidfd is acquired and before it is published. Signals to
// bubblewrap are recorded but not sent, so its death cannot end the init:
// only the publication's own signal to the init can. Spawn must then fail
// once the init has exited, leaking nothing.
func TestADeadlineDuringStartupKillsTheInit(t *testing.T) {
	noLeak(t)
	var mu sync.Mutex
	var asked []string // "bwrap" or "init", in order
	sendKill = func(fd int, init bool) error {
		mu.Lock()
		defer mu.Unlock()
		if !init {
			asked = append(asked, "bwrap")
			return nil // recorded, not sent
		}
		asked = append(asked, "init")
		return unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
	}
	defer func() {
		sendKill = func(fd int, _ bool) error { return unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0) }
	}()
	held := -1
	startupHook = func(p *Process, initFD int) {
		held, _ = unix.Dup(initFD)
		// Wait for the deadline's kill request, which saw no init yet.
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			mu.Lock()
			n := len(asked)
			mu.Unlock()
			if n > 0 && p.Expired() {
				return
			}
		}
	}
	defer func() { startupHook = nil }()
	spec := stubSpec(t, "echo")
	spec.Deadline = 50 * time.Millisecond
	type spawned struct {
		p   *Process
		err error
	}
	done := make(chan spawned, 1)
	go func() { p, err := Spawn(spec); done <- spawned{p, err} }()
	var got spawned
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		// Bounded failure: the init still runs, so Spawn is still waiting.
		// Kill it through the held pidfd, which ends bubblewrap too.
		_ = unix.PidfdSendSignal(held, unix.SIGKILL, nil, 0)
		<-done
		unix.Close(held)
		t.Fatal("Spawn hung after its deadline: publication did not signal the init")
	}
	if got.err == nil || got.p != nil {
		got.p.Kill()
		t.Fatalf("Spawn after its deadline returned %v, %v", got.p, got.err)
	}
	if held < 0 {
		t.Fatal("the hook got no init pidfd")
	}
	defer unix.Close(held)
	mu.Lock()
	order := strings.Join(asked, ",")
	mu.Unlock()
	if !strings.HasPrefix(order, "bwrap,") || !strings.Contains(order, "init") {
		t.Fatalf("kill requests %q: want the deadline's (bubblewrap only), then the init's at publication", order)
	}
	if survivors(t, []int{held}) != 0 {
		t.Fatal("the init survived a deadline that fired during startup")
	}
}

func TestWaitWithoutTheInitsExitIsUnverified(t *testing.T) {
	if verified(-1) {
		t.Fatal("no init pidfd was taken as verified")
	}
	for name, fake := range map[string]func([]unix.PollFd, int) (int, error){
		"poll fails": func([]unix.PollFd, int) (int, error) { return 0, unix.EIO },
		"no POLLIN":  func(fds []unix.PollFd, _ int) (int, error) { fds[0].Revents = unix.POLLHUP; return 1, nil },
	} {
		poll = fake
		p := spawn(t, stubSpec(t, "echo"))
		p.Stdin().Close()
		err := waitWithin(t, p, 10*time.Second)
		poll = unix.Poll // the waiter is done with it
		if !errors.Is(err, ErrUnverified) {
			t.Fatalf("%s: Wait returned %v, want ErrUnverified", name, err)
		}
	}
}

// TestTheInitsParentIsVerified: a pid that is not bubblewrap's child is
// refused, and its pidfd closed.
func TestTheInitsParentIsVerified(t *testing.T) {
	noLeak(t)
	if fd := openChild(os.Getpid(), os.Getpid()); fd >= 0 {
		t.Fatal("a process was taken as its own child")
	}
	if fd := openChild(1<<22+1, os.Getpid()); fd >= 0 { // above pid_max
		t.Fatal("a pid that cannot exist was opened")
	}
	openInit = func(pid, parent int) int { return openChild(pid, parent+1) }
	defer func() { openInit = openChild }()
	if p, err := Spawn(stubSpec(t, "echo")); err == nil || p != nil {
		t.Fatalf("Spawn with an init of another parent returned %v, %v", p, err)
	}
}

// TestRepeatedSpawnsReleaseEverything: failed spawns and killed ones, many
// times over, leave no descriptor or goroutine behind.
func TestRepeatedSpawnsReleaseEverything(t *testing.T) {
	noLeak(t)
	for range 10 {
		openInit = func(int, int) int { return -1 }
		_, err := Spawn(stubSpec(t, "echo"))
		openInit = openChild
		if err == nil {
			t.Fatal("Spawn without the init succeeded")
		}
		p, err := Spawn(stubSpec(t, "echo"))
		if err != nil {
			t.Fatal(err)
		}
		p.Kill()
		_ = p.Wait()
		p.Stdout().Close()
	}
}
