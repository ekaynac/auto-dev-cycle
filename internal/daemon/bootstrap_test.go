// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"os"
	"path/filepath"
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
