// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// shortDir is a private directory with a short path, so a client can dial
// a socket in it by name (sun_path holds 108 bytes).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "adcd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// runDir is a shared run directory of mode perm and this process's group.
func runDir(t *testing.T, perm os.FileMode) string {
	t.Helper()
	d := filepath.Join(shortDir(t), "run")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, perm); err != nil {
		t.Fatal(err)
	}
	return d
}

func egid() string { return strconv.Itoa(os.Getegid()) }

func lstat(t *testing.T, p string) *syscall.Stat_t {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		t.Fatalf("lstat: %v", err)
	}
	return &st
}

func gone(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s is still there: %v", filepath.Base(p), err)
	}
}

// The private socket is 0600 and this user's; the shared socket is off by
// default and, when configured, 0660 with the configured group.
func TestSocketModes(t *testing.T) {
	s := serveStore(t, defaultLimits, nil)
	st := lstat(t, filepath.Join(s.dir, privateSocket))
	if st.Mode&syscall.S_IFMT != syscall.S_IFSOCK || st.Mode&0o777 != 0o600 || int(st.Uid) != os.Geteuid() {
		t.Errorf("private socket mode %o uid %d", st.Mode, st.Uid)
	}
	if len(s.srv.sockets) != 1 {
		t.Errorf("%d sockets without a shared configuration", len(s.srv.sockets))
	}
	if cfg, err := LoadConfig([]string{"--state-dir", stateDir(t)}); err != nil || cfg.Shared != nil {
		t.Errorf("the shared socket is on by default: %+v %v", cfg, err)
	}

	run := runDir(t, 0o750)
	s = serveStore(t, defaultLimits, &SharedSocket{RunDir: run, Group: egid()})
	st = lstat(t, filepath.Join(run, sharedSocket))
	if st.Mode&syscall.S_IFMT != syscall.S_IFSOCK || st.Mode&0o777 != 0o660 || int(st.Gid) != os.Getegid() || int(st.Uid) != os.Geteuid() {
		t.Errorf("shared socket mode %o uid %d gid %d", st.Mode, st.Uid, st.Gid)
	}
	if resp := s.get(t, unixClient(filepath.Join(run, sharedSocket)), api.VersionPath, ""); resp.StatusCode != 200 {
		t.Errorf("version over the shared socket: %s", resp.Status)
	}
}

// A run directory the daemon does not control is refused, as is an
// explicit configuration that is incomplete or names no group.
func TestRunDirRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns every directory")
	}
	good := runDir(t, 0o750)
	link := filepath.Join(shortDir(t), "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	for name, sh := range map[string]SharedSocket{
		"world-writable":       {runDir(t, 0o777), egid()},
		"group-writable":       {runDir(t, 0o770), egid()},
		"not group-searchable": {runDir(t, 0o700), egid()},
		"foreign-owned":        {"/usr", egid()},
		"another group":        {good, "0"},
		"a link":               {link, egid()},
		"missing":              {filepath.Join(good, "absent"), egid()},
	} {
		dir := shortDir(t)
		w := seed(t, dir, daemonProjection(t))
		srv, err := listen(w.s, dir, &sh, defaultLimits)
		if !errors.Is(err, ErrConfig) {
			t.Errorf("%s: accepted: %v", name, err)
			srv.close()
		}
		w.done()
		gone(t, filepath.Join(dir, privateSocket)) // a refused start leaves no socket
	}
	// Through Run: the refusal comes after startup, which it unwinds.
	state := stateDir(t)
	err := Run(context.Background(), Config{StateDir: state, Shared: &SharedSocket{RunDir: runDir(t, 0o777), Group: egid()}}, io.Discard, nil)
	if !errors.Is(err, ErrConfig) {
		t.Errorf("Run with a world-writable run dir: %v", err)
	}
	lockFree(t, state)
	gone(t, filepath.Join(state, privateSocket))
	for name, sh := range map[string]*SharedSocket{
		"no group":         {RunDir: good},
		"no run dir":       {Group: egid()},
		"relative":         {RunDir: "run", Group: egid()},
		"in the state":     {RunDir: state + "/run", Group: egid()},
		"no such group":    {RunDir: good, Group: "adc-no-such-group"},
		"the state itself": {RunDir: state, Group: egid()},
	} {
		if err := (Config{StateDir: state, Shared: sh}).Validate(); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: accepted: %v", name, err)
		}
	}
	if _, err := LoadConfig([]string{"--state-dir", state, "--shared-group", egid()}); !errors.Is(err, ErrConfig) {
		t.Errorf("a group without a run dir: %v", err)
	}
	if cfg, err := LoadConfig([]string{"--state-dir", state, "--shared-run-dir", good, "--shared-group", egid()}); err != nil || cfg.Shared == nil {
		t.Errorf("a complete shared configuration: %+v %v", cfg, err)
	}
}

// A stale socket of this user's is replaced; a live one, a file, a link
// or a directory at the path is refused and left as it was.
func TestStaleSocket(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, privateSocket)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	w := seed(t, dir, daemonProjection(t))
	defer w.done()
	// Live: someone answers on it.
	if _, err := listen(w.s, dir, nil, defaultLimits); !errors.Is(err, ErrConfig) {
		t.Fatalf("a live socket was replaced: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil { // marks the stale socket (an inode number may be reused)
		t.Fatal(err)
	}
	ln.Close()
	srv, err := listen(w.s, dir, nil, defaultLimits)
	if err != nil {
		t.Fatalf("a stale socket was not replaced: %v", err)
	}
	if lstat(t, path).Mode&0o777 != 0o600 {
		t.Error("the stale socket is still the one at the path")
	}
	if err := srv.close(); err != nil {
		t.Fatal(err)
	}
	gone(t, path)

	for name, make := range map[string]func() error{
		"a file":      func() error { return os.WriteFile(path, []byte("keep"), 0o600) },
		"a link":      func() error { return os.Symlink(filepath.Join(dir, "elsewhere"), path) },
		"a directory": func() error { return os.Mkdir(path, 0o700) },
	} {
		if err := make(); err != nil {
			t.Fatal(err)
		}
		before := lstat(t, path)
		if srv, err := listen(w.s, dir, nil, defaultLimits); !errors.Is(err, ErrConfig) {
			t.Errorf("%s at the path: %v", name, err)
			srv.close()
		}
		if after := lstat(t, path); after.Ino != before.Ino || after.Mode != before.Mode {
			t.Errorf("%s at the path was touched", name)
		}
		if b, err := os.ReadFile(path); name == "a file" && (err != nil || string(b) != "keep") {
			t.Errorf("the file changed: %q %v", b, err)
		}
		os.Remove(path)
	}
	sock := unix.Stat_t{Mode: unix.S_IFSOCK, Uid: uint32(os.Geteuid())}
	if !staleCandidate(sock, os.Geteuid()) || staleCandidate(sock, os.Geteuid()+1) {
		t.Error("only this user's socket is a stale candidate")
	}
}

// M3: the private socket admits only the daemon's uid; a refused peer's
// connection is closed before a request is read. The admitted connection
// carries its listener and uid to the request context.
func TestPeerCredentials(t *testing.T) {
	uid := os.Geteuid()
	for _, c := range []struct {
		via      identity.Listener
		peer     int
		admitted bool
	}{{identity.PrivateSocket, uid, true}, {identity.PrivateSocket, uid + 1, false}, {identity.SharedSocket, uid + 1, true},
		{identity.HTTPS, uid, false}, {0, uid, false}} {
		if admits(c.via, c.peer, uid) != c.admitted {
			t.Errorf("admits(%d, %d) != %v", c.via, c.peer, c.admitted)
		}
	}
	for _, daemon := range []int{uid, uid + 1} {
		ln, err := net.Listen("unix", filepath.Join(shortDir(t), "s"))
		if err != nil {
			t.Fatal(err)
		}
		pl := peerListener{Listener: ln, via: identity.PrivateSocket, daemon: daemon}
		accepted := make(chan net.Conn, 1)
		go func() {
			c, err := pl.Accept()
			if err == nil {
				accepted <- c
			}
		}()
		c, err := net.Dial("unix", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if daemon == uid {
			got := <-accepted
			p, ok := api.PeerOf(connContext(context.Background(), got))
			if !ok || p.Via != identity.PrivateSocket || p.UID != uid {
				t.Errorf("peer %+v %v", p, ok)
			}
			got.Close()
		} else {
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Errorf("a foreign peer was not closed: %v", err)
			}
		}
		c.Close()
		ln.Close()
	}
}
