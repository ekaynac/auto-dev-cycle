// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
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
