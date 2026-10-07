// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/apidoc"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

func bootState(t *testing.T, d *Daemon) identity.BootstrapState {
	t.Helper()
	var st identity.BootstrapState
	if err := d.store.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		st, err = identity.BootstrapOf(tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

func readSecret(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, bootstrapFile))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// The first start issues the secret: a 0600 regular file of this user and
// its record; a restart neither renews nor recreates it, and says so.
func TestBootstrapSecretIssuedOnce(t *testing.T) {
	dir := stateDir(t)
	d, err := Start(context.Background(), Config{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, bootstrapFile)
	if st := lstat(t, p); st.Mode&0o170777 != 0o100600 || int(st.Uid) != os.Geteuid() {
		t.Fatalf("bootstrap.secret mode %o uid %d", st.Mode, st.Uid)
	}
	first, secret := bootState(t, d), readSecret(t, dir)
	if first.Consumed || first.Secret == nil || !first.Live(time.Now()) || d.note != notePending {
		t.Fatalf("first start: %+v, note %q", first, d.note)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d = restart(t, dir)
	if again := bootState(t, d); *again.Secret != *first.Secret || readSecret(t, dir) != secret || d.store == nil || d.note != notePending {
		t.Fatalf("a restart renewed the secret: %+v", again)
	}
	if h := d.Head().Seq; h != 2 { // the system actor and the secret's record, nothing since
		t.Fatalf("head %d", h)
	}
	// Past the expiry, a start only says so.
	if _, note, err := openBootstrap(context.Background(), d.store, d.system, dir, func() time.Time { return time.Now().Add(25 * time.Hour) }); err != nil || note != noteExpired {
		t.Fatalf("expired: %q %v", note, err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	// A live record whose file is gone is not recreated.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	d = restart(t, dir)
	defer d.Close()
	if d.note != noteMissing || *bootState(t, d).Secret != *first.Secret || d.Head().Seq != 2 {
		t.Fatalf("missing file: note %q", d.note)
	}
	gone(t, p)
}

func restart(t *testing.T, dir string) *Daemon {
	t.Helper()
	d, err := Start(context.Background(), Config{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// What sits at bootstrap.secret before the first secret is recorded (a
// file left by a crash before its event, or a planted symlink) is never
// trusted: it is replaced by a fresh 0600 regular file, and a link's
// target is left untouched.
func TestBootstrapReplacesUnrecordedFile(t *testing.T) {
	const planted = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	for name, plant := range map[string]func(t *testing.T, p, target string){
		"a leftover file": func(t *testing.T, p, _ string) {
			if err := os.WriteFile(p, []byte(planted+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a symlink": func(t *testing.T, p, target string) {
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := stateDir(t)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte(planted), 0o644); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dir, bootstrapFile)
			plant(t, p, target)
			d := restart(t, dir)
			defer d.Close()
			secret := readSecret(t, dir)
			if st := lstat(t, p); st.Mode&0o170777 != 0o100600 || secret == planted || bootState(t, d).Secret == nil {
				t.Fatalf("not replaced: mode %o", st.Mode)
			}
			if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(secret) {
				t.Fatal("the new secret is not of the issued form")
			}
			if b, err := os.ReadFile(target); err != nil || string(b) != planted {
				t.Fatalf("the link target changed: %v", err)
			}
		})
	}
}

// running is a daemon serving its sockets.
type running struct {
	dir    string
	d      *Daemon
	logs   *logBuffer
	stop   func()
	shared string
}

func runBootstrap(t *testing.T) *running {
	t.Helper()
	r := &running{dir: shortDir(t), logs: &logBuffer{}}
	run := runDir(t, 0o750)
	r.shared = filepath.Join(run, sharedSocket)
	ctx, cancel := context.WithCancel(context.Background())
	done, ready := make(chan error, 1), make(chan *Daemon, 1)
	go func() {
		done <- Run(ctx, Config{StateDir: r.dir, Shared: &SharedSocket{RunDir: run, Group: egid()}}, r.logs, func(d *Daemon) { ready <- d })
	}()
	select {
	case r.d = <-ready:
	case err := <-done:
		t.Fatalf("the daemon did not start: %v", err)
	}
	var once sync.Once
	r.stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(r.stop)
	return r
}

func (r *running) post(t *testing.T, sock, key, secret string) (int, types.BootstrapResponseVariant) {
	t.Helper()
	body, err := json.Marshal(types.BootstrapRequest{IdempotencyKey: key, Secret: secret, Name: "first"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := unixClient(sock).Post("http://adcd"+apidoc.BootstrapPath, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := contract.Decode[types.BootstrapResponse]("api/bootstrap-response", raw)
	if err != nil {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, out.Variant
}

func (r *running) private() string { return filepath.Join(r.dir, privateSocket) }

func refusedWith(t *testing.T, v types.BootstrapResponseVariant, reason vocab.Reason) {
	t.Helper()
	if f, ok := v.(*types.BootstrapRefused); !ok || f.Reason != reason {
		t.Fatalf("want refused %s, got %+v", reason, v)
	}
}

// A wrong secret, an expired one and the shared socket are refused; the
// right one creates the first operator and its token, delivered once, and
// deletes the file; a replay delivers nothing; a leftover file (a crash
// after the commit) never bootstraps again, and the next start removes it.
// No secret reaches the log or the state directory.
func TestBootstrapEndpoint(t *testing.T) {
	r := runBootstrap(t)
	secret := readSecret(t, r.dir)
	wrong, _, err := identity.NewBootstrapSecret()
	if err != nil {
		t.Fatal(err)
	}
	if code, v := r.post(t, r.shared, "k-shared", secret); code != http.StatusForbidden || v != nil {
		t.Fatalf("shared socket: %d %+v", code, v)
	}
	if code, _ := r.post(t, r.private(), "k-bad", "not a secret"); code != http.StatusBadRequest {
		t.Fatalf("malformed: %d", code)
	}
	_, v := r.post(t, r.private(), "k-wrong", wrong.Reveal())
	refusedWith(t, v, vocab.ReasonConfirmationRefused)
	r.d.boot.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	_, v = r.post(t, r.private(), "k-late", secret)
	refusedWith(t, v, vocab.ReasonConfirmationRefused)
	r.d.boot.now = time.Now
	// The same key with another secret is another request.
	if code, v := r.post(t, r.private(), "k-wrong", secret); code != http.StatusConflict {
		t.Fatalf("reused key: %d %+v", code, v)
	}
	_, v = r.post(t, r.private(), "k-ok", secret)
	ok, isApplied := v.(*types.BootstrapApplied)
	if !isApplied || ok.Replayed || ok.IssuedCredential == nil {
		t.Fatalf("bootstrap: %+v", v)
	}
	bearer := *ok.IssuedCredential
	gone(t, filepath.Join(r.dir, bootstrapFile))
	if err := r.d.store.View(context.Background(), func(tx *eventstore.Tx) error {
		a, err := identity.Authenticate(tx, bearer)
		if err == nil && (a.ID != ok.Operator || a.Kind != types.ActorKindOperator || !bootState(t, r.d).Consumed) {
			t.Errorf("the token is %+v's", a)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The system actions are the system principal's alone, an operator's
	// included.
	var p identity.Principal
	if err := r.d.store.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		p, err = identity.Authenticated(tx, bearer, identity.PrivateSocket)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.store.Submit(context.Background(), p.Command(newID(t, "cmd"), issueAction, "k", []byte(`{}`), nil), nil); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("an operator's system action: %v", err)
	}
	_, v = r.post(t, r.private(), "k-ok", secret)
	if again, isApplied := v.(*types.BootstrapApplied); !isApplied || !again.Replayed || again.IssuedCredential != nil || again.Token != ok.Token {
		t.Fatalf("replay: %+v", v)
	}
	if err := os.WriteFile(filepath.Join(r.dir, bootstrapFile), []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, v = r.post(t, r.private(), "k-after", secret)
	refusedWith(t, v, vocab.ReasonConfirmationRefused)
	_, bearerSecret, _ := strings.Cut(bearer, ".")
	secretsAbsent(t, r, secret, bearerSecret)
	r.stop()
	secretsAbsent(t, r, secret, bearerSecret)
	d := restart(t, r.dir)
	defer d.Close()
	gone(t, filepath.Join(r.dir, bootstrapFile))
}

// secretsAbsent: neither secret is in the log or in any file of the state
// directory (the database, its WAL), the leftover secret file aside.
func secretsAbsent(t *testing.T, r *running, secrets ...string) {
	t.Helper()
	err := filepath.WalkDir(r.dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || !e.Type().IsRegular() || e.Name() == bootstrapFile {
			return err
		}
		b, err := os.ReadFile(p)
		for _, s := range secrets {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("%s holds a secret", e.Name())
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if strings.Contains(r.logs.String(), s) {
			t.Error("the log holds a secret")
		}
	}
}

// Two clients racing with different keys: exactly one bootstraps.
func TestBootstrapRace(t *testing.T) {
	r := runBootstrap(t)
	secret := readSecret(t, r.dir)
	var wg sync.WaitGroup
	results := make([]types.BootstrapResponseVariant, 2)
	for i := range results {
		wg.Go(func() { _, results[i] = r.post(t, r.private(), "race-"+string(rune('a'+i)), secret) })
	}
	wg.Wait()
	applied := 0
	for _, v := range results {
		if a, ok := v.(*types.BootstrapApplied); ok && a.IssuedCredential != nil {
			applied++
		} else {
			refusedWith(t, v, vocab.ReasonConfirmationRefused)
		}
	}
	if applied != 1 {
		t.Fatalf("%d bootstraps", applied)
	}
}

// A request that arrives while the secret is live but reaches the writer
// after it expired is refused: expiry is judged inside the transaction.
// The writer is held; the clock moves past the expiry once the request has
// read it on arrival; then the writer is released.
func TestBootstrapExpiresWhileWaiting(t *testing.T) {
	r := runBootstrap(t)
	secret := readSecret(t, r.dir)
	var late atomic.Bool
	arrived := make(chan struct{})
	var once sync.Once
	r.d.boot.now = func() time.Time {
		if late.Load() {
			return time.Now().Add(25 * time.Hour)
		}
		once.Do(func() { close(arrived) })
		return time.Now()
	}
	held, release := make(chan struct{}), make(chan struct{})
	hold := make(chan error, 1)
	go func() {
		cmd := newID(t, "cmd")
		_, err := r.d.store.Submit(context.Background(), r.d.system.Command(cmd, issueAction, cmd, []byte(`{}`), nil),
			func(*eventstore.Tx) (eventstore.Decision, error) {
				close(held)
				<-release
				return eventstore.Decision{}, errors.New("rolled back")
			})
		hold <- err
	}()
	<-held
	result := make(chan types.BootstrapResponseVariant, 1)
	go func() {
		_, v := r.post(t, r.private(), "k-wait", secret)
		result <- v
	}()
	<-arrived
	late.Store(true)
	close(release)
	if err := <-hold; err == nil {
		t.Fatal("the held command committed")
	}
	refusedWith(t, <-result, vocab.ReasonConfirmationRefused)
	if st := bootState(t, r.d); st.Consumed {
		t.Fatal("bootstrapped after the expiry")
	}
}
