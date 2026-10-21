// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// command runs an offline command and returns its exit code and stderr.
func command(cmd func([]string, io.Writer) int, args ...string) (int, string) {
	var out bytes.Buffer
	code := cmd(args, &out)
	return code, out.String()
}

// people is a seeded store's cast: the first operator with two tokens,
// another operator, a supervisor (one token each), a revoked operator and
// the system actor.
type people struct {
	dir                     string
	sys, op, other, sup, rv types.ActorID
	opTokens                []string
	otherToken, supToken    string
}

func recoveryStore(t *testing.T) people {
	t.Helper()
	p := people{dir: stateDir(t)}
	w := seed(t, p.dir, daemonProjection(t))
	actor := func(kind types.ActorKind, by types.ActorID) types.ActorID {
		a := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: kind, Name: string(kind), CreatedBy: by}
		if by == "" {
			a.CreatedBy = a.ID
		}
		w.do(a.CreatedBy, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(a) })
		return a.ID
	}
	token := func(holder, by types.ActorID) string {
		var s identity.Secret
		w.do(by, func(*eventstore.Tx) (evs []eventstore.Event, err error) {
			evs, s, err = identity.IssueToken(holder, by, time.Now())
			return evs, err
		})
		return s.Reveal()
	}
	p.sys = actor(types.ActorKindSystem, "")
	p.op = actor(types.ActorKindOperator, p.sys)
	p.opTokens = []string{token(p.op, p.sys), token(p.op, p.op)}
	p.other, p.sup, p.rv = actor(types.ActorKindOperator, p.op), actor(types.ActorKindSupervisor, p.op), actor(types.ActorKindOperator, p.op)
	p.otherToken, p.supToken = token(p.other, p.op), token(p.sup, p.op)
	token(p.rv, p.op)
	w.do(p.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return identity.RevokeActor(tx, p.rv, time.Now()) })
	w.done()
	return p
}

// noSecret: no file of the state directory (the database and its WAL
// included) and no output holds any of secrets.
func noSecret(t *testing.T, dir string, outputs []string, secrets ...string) {
	t.Helper()
	if err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || !e.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		for _, s := range secrets {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("%s holds a secret", e.Name())
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, o := range outputs {
		for _, s := range secrets {
			if strings.Contains(o, s) {
				t.Error("the output holds a secret")
			}
		}
	}
}

func privateDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "out")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

// offlineArgs are each offline command's arguments on p's store.
func offlineArgs(p people, out string) map[[2]string][]string {
	return map[[2]string][]string{
		{"bootstrap", "renew"}:        {"--state-dir", p.dir},
		{"recover", "operator-token"}: {"--state-dir", p.dir, "--actor", string(p.op), "--out", out},
	}
}

// Every offline command is refused while the daemon holds the lock.
func TestOfflineRefusedWhileRunning(t *testing.T) {
	p := recoveryStore(t)
	d, err := Start(context.Background(), Config{StateDir: p.dir})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	out := filepath.Join(privateDir(t), "token")
	for words, args := range offlineArgs(p, out) {
		if code, msg := command(Commands[words], args...); code != entry.ExitCouldNotRun || !strings.Contains(msg, "could_not_run/controller_already_running") {
			t.Errorf("%v while running: %d %s", words, code, msg)
		}
	}
	gone(t, out)
}

// A store failing any startup trust check (a broken chain, a trusted head
// it does not reach, drifted projections, an event kind this build does
// not read) is refused by every offline command, which append nothing and release
// the lock.
func TestOfflineRefusesUntrustedStore(t *testing.T) {
	unread := func(t *testing.T) (people, []string) {
		p := people{dir: stateDir(t), op: types.ActorID(newID(t, "act"))}
		w := seed(t, p.dir, daemonProjection(t), "actor.created/v9")
		a := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
		a.CreatedBy = a.ID
		w.do(a.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
			evs, err := identity.CreateActor(a)
			evs[0].Kind = "actor.created/v9"
			return evs, err
		})
		w.done()
		return p, nil
	}
	sqlCase := func(stmts ...string) func(t *testing.T) (people, []string) {
		return func(t *testing.T) (people, []string) {
			p := recoveryStore(t)
			execSQL(t, p.dir, stmts...)
			return p, nil
		}
	}
	for name, c := range map[string]struct {
		make func(t *testing.T) (people, []string)
		want string
	}{
		"broken chain": {sqlCase(`DROP TRIGGER events_append_only_update`, `UPDATE events SET at = '2020-01-01T00:00:00Z' WHERE seq = 1`), "failed/log_integrity_broken"},
		"trusted head": {func(t *testing.T) (people, []string) {
			p := recoveryStore(t)
			return p, []string{"--trusted-head", "999:sha256:" + strings.Repeat("0", 64)}
		}, "failed/log_integrity_broken"},
		"projection drift": {sqlCase(`UPDATE proj_kv SET value = json_set(value, '$.revoked_at', NULL) WHERE projection = 'actors' AND value LIKE '%revoked_at":"%'`), "could_not_run/ambiguous_state"},
		"unread kind":      {unread, "could_not_run/store_unreadable"},
	} {
		t.Run(name, func(t *testing.T) {
			p, extra := c.make(t)
			seq := lastSeq(t, p.dir)
			out := filepath.Join(privateDir(t), "token")
			for words, args := range offlineArgs(p, out) {
				code, msg := command(Commands[words], append(args, extra...)...)
				if code == entry.ExitOK || !strings.Contains(msg, c.want) {
					t.Errorf("%v: %d %s", words, code, msg)
				}
				lockFree(t, p.dir)
			}
			gone(t, out)
			if lastSeq(t, p.dir) != seq {
				t.Fatal("a refused command appended to the log")
			}
		})
	}
}

// bootRecord is the store's bootstrap secret record, read offline.
func bootRecord(t *testing.T, dir string) (identity.BootstrapState, int64) {
	t.Helper()
	d, err := start(context.Background(), Config{StateDir: dir}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	return bootState(t, d), d.Head().Seq
}

// Renewal, before consumption, writes a fresh secret and records its new
// hash and 24 h expiry every time; the old secret no longer bootstraps; a
// restart does not renew again; after consumption it is refused.
func TestRenewBootstrap(t *testing.T) {
	dir := stateDir(t)
	d := restart(t, dir)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	secrets := []string{readSecret(t, dir)}
	before, _ := bootRecord(t, dir)
	hashes := []string{before.Secret.Hash}
	var outputs []string
	for range 2 {
		at := time.Now()
		code, msg := command(RenewCommand, "--state-dir", dir)
		if code != entry.ExitOK {
			t.Fatalf("renew: %d %s", code, msg)
		}
		st, _ := bootRecord(t, dir)
		expires, err := time.Parse(time.RFC3339Nano, string(st.Secret.ExpiresAt))
		if err != nil || expires.Before(at.Add(24*time.Hour)) || expires.After(time.Now().Add(24*time.Hour)) || st.Consumed {
			t.Fatalf("renewed record %+v", st)
		}
		if st := lstat(t, filepath.Join(dir, bootstrapFile)); st.Mode&0o170777 != 0o100600 {
			t.Fatalf("bootstrap.secret mode %o", st.Mode)
		}
		secrets, hashes, outputs = append(secrets, readSecret(t, dir)), append(hashes, st.Secret.Hash), append(outputs, msg)
	}
	for i := 1; i < len(hashes); i++ {
		if hashes[i] == hashes[i-1] || secrets[i] == secrets[i-1] {
			t.Fatal("a renewal reused the secret")
		}
	}
	// A restart neither renews nor rewrites.
	_, seq := bootRecord(t, dir)
	d = restart(t, dir)
	if st := bootState(t, d); st.Secret.Hash != hashes[2] || readSecret(t, dir) != secrets[2] || d.Head().Seq != seq {
		t.Fatalf("a restart renewed: %+v", st)
	}
	for i, s := range []string{secrets[0], secrets[1], secrets[2]} {
		parsed, err := identity.ParseBootstrapSecret(s)
		if err != nil {
			t.Fatal(err)
		}
		_, v := d.boot.consume(context.Background(), types.BootstrapRequest{IdempotencyKey: "k" + string(rune('a'+i)), Secret: s, Name: "first"}, parsed)
		if _, applied := v.(*types.BootstrapApplied); applied != (i == 2) {
			t.Fatalf("secret %d: %+v", i, v)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	_, seq = bootRecord(t, dir)
	if code, msg := command(RenewCommand, "--state-dir", dir); code != entry.ExitRefused || !strings.Contains(msg, "failed/confirmation_refused") {
		t.Fatalf("renew after consumption: %d %s", code, msg)
	}
	gone(t, filepath.Join(dir, bootstrapFile))
	if _, after := bootRecord(t, dir); after != seq {
		t.Fatal("a refused renewal appended to the log")
	}
	noSecret(t, dir, outputs, secrets...)
}

// tree is every entry under dir with its type, mode and size.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err == nil {
			out[p] = fmt.Sprintf("%v %d", info.Mode(), info.Size())
		}
		return err
	}); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return out
}

// An offline command never creates a store or any part of one: a missing
// state directory, an empty one, one with only a lock file, one with an
// uninitialized database, or a store without its blob directories is
// refused and left exactly as it was; an
// initialized store without a system actor is refused too.
func TestOfflineNeedsExistingStore(t *testing.T) {
	for name, c := range map[string]struct {
		make func(t *testing.T, dir string)
		want string
	}{
		"missing": {func(*testing.T, string) {}, "the state directory must be"},
		"empty":   {func(t *testing.T, dir string) { mkdir(t, dir) }, "no initialized store"},
		"lock only": {func(t *testing.T, dir string) {
			mkdir(t, dir)
			writeFile0600(t, filepath.Join(dir, eventstore.LockFile), "")
		}, "no initialized store"},
		"empty database": {func(t *testing.T, dir string) {
			mkdir(t, dir)
			writeFile0600(t, filepath.Join(dir, eventstore.LockFile), "")
			writeFile0600(t, filepath.Join(dir, eventstore.DBFile), "")
		}, "failed/log_integrity_broken"},
		"no blob directories": {func(t *testing.T, dir string) {
			restart(t, dir).Close()
			if err := os.RemoveAll(filepath.Join(dir, "blobs")); err != nil {
				t.Fatal(err)
			}
		}, "no initialized store"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := stateDir(t)
			c.make(t, dir)
			before := tree(t, filepath.Dir(dir))
			if code, msg := command(RenewCommand, "--state-dir", dir); code == entry.ExitOK || !strings.Contains(msg, c.want) {
				t.Fatalf("%d %s", code, msg)
			}
			if after := tree(t, filepath.Dir(dir)); fmt.Sprint(after) != fmt.Sprint(before) {
				t.Fatalf("the filesystem changed:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
	dir := stateDir(t)
	seed(t, dir, daemonProjection(t)).done()
	if code, msg := command(RenewCommand, "--state-dir", dir); code != entry.ExitCouldNotRun || !strings.Contains(msg, "no system actor") {
		t.Fatalf("no system actor: %d %s", code, msg)
	}
	if lastSeq(t, dir) != 0 {
		t.Fatal("an offline command recorded a system actor")
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeFile0600(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The capacity history is checked offline on its own: a lease granted
// before capacity was wired is refused as at startup.
func TestOfflineChecksCapacityHistory(t *testing.T) {
	dir := stateDir(t)
	w := seed(t, dir, leasesOnly)
	w.grantedLease(types.ActorID(newID(t, "act")), 4)
	w.done()
	if code, msg := command(RenewCommand, "--state-dir", dir); code != entry.ExitCouldNotRun || !strings.Contains(msg, "before capacity was wired") {
		t.Fatalf("%d %s", code, msg)
	}
}

// An offline command never runs the restart transition; adcd run does.
func TestOfflineMarksNoPendingEffect(t *testing.T) {
	dir := stateDir(t)
	calls := 0
	markPending = func(s *eventstore.Store, ctx context.Context, actor string) (int, error) {
		calls++
		return s.MarkPendingUncertain(ctx, actor)
	}
	t.Cleanup(func() { markPending = (*eventstore.Store).MarkPendingUncertain })
	d := restart(t, dir) // created: the transition too (#226 entry 64)
	if err := d.Close(); err != nil || calls != 1 {
		t.Fatalf("a fresh start ran %d transition(s): %v", calls, err)
	}
	if code, msg := command(RenewCommand, "--state-dir", dir); code != entry.ExitOK || calls != 1 {
		t.Fatalf("renew: %d %s, %d transition(s)", code, msg, calls)
	}
	d = restart(t, dir)
	if err := d.Close(); err != nil || calls != 2 {
		t.Fatalf("a restart ran %d transition(s): %v", calls, err)
	}
}

// A renewal that fails leaves the recorded secret authoritative: a file
// that cannot be replaced records nothing and is left in place; a commit
// the projector refuses rolls back (the file then holds an unrecorded
// secret that never bootstraps), and a retry renews with a fresh secret.
// The diagnostics are fixed text.
func TestRenewFailures(t *testing.T) {
	const failed = "adcd: could_not_run/store_unreadable: the bootstrap secret cannot be renewed\n"
	dir := stateDir(t)
	d := restart(t, dir)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	first, seq := bootRecord(t, dir)
	p := filepath.Join(dir, bootstrapFile)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	mkdir(t, p)
	if code, msg := command(RenewCommand, "--state-dir", dir); code != entry.ExitCouldNotRun || msg != failed {
		t.Fatalf("file failure: %d %q", code, msg)
	}
	if st, s := bootRecord(t, dir); st.Secret.Hash != first.Secret.Hash || s != seq || lstat(t, p).Mode&0o170000 != 0o040000 {
		t.Fatal("a failed file replacement changed the state")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	renewRecord = func(system types.ActorID, hash string, now time.Time) ([]eventstore.Event, error) {
		return identity.RenewBootstrap(system, hash, now.Add(-48*time.Hour)) // expired before it is recorded
	}
	t.Cleanup(func() { renewRecord = identity.RenewBootstrap })
	code, msg := command(RenewCommand, "--state-dir", dir)
	renewRecord = identity.RenewBootstrap
	if code != entry.ExitCouldNotRun || msg != failed {
		t.Fatalf("rollback: %d %q", code, msg)
	}
	unrecorded := readSecret(t, dir)
	if st, s := bootRecord(t, dir); st.Secret.Hash != first.Secret.Hash || s != seq {
		t.Fatal("a rolled-back renewal changed the record")
	}
	if code, msg := command(RenewCommand, "--state-dir", dir); code != entry.ExitOK {
		t.Fatalf("retry: %d %s", code, msg)
	}
	st, _ := bootRecord(t, dir)
	if st.Secret.Hash == first.Secret.Hash || readSecret(t, dir) == unrecorded {
		t.Fatal("the retry did not renew with a fresh secret")
	}
	d = restart(t, dir)
	defer d.Close()
	parsed, err := identity.ParseBootstrapSecret(unrecorded)
	if err != nil {
		t.Fatal(err)
	}
	if _, v := d.boot.consume(context.Background(), types.BootstrapRequest{IdempotencyKey: "k", Secret: unrecorded, Name: "first"}, parsed); v == nil {
		t.Fatal("no response")
	} else if _, applied := v.(*types.BootstrapApplied); applied {
		t.Fatal("an unrecorded secret bootstrapped")
	}
}

// lastOf is the time and payload of the last event of kind, read directly.
func lastOf(t *testing.T, dir, kind string) (at string, payload []byte) {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(dir, eventstore.DBFile), RawQuery: "mode=ro"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT at, payload FROM events WHERE kind = ? ORDER BY seq DESC LIMIT 1`, kind).Scan(&at, &payload); err != nil {
		t.Fatal(err)
	}
	return at, payload
}

// The offline commands run on the daemon's one store clock. A renewal is
// made and expires at its command's time (Tx.Now), so a clock that steps
// back past the last event still records a live secret made at its event's
// time; a recovered token is minted at the store clock's time.
func TestOfflineOnTheStoreClock(t *testing.T) {
	dir := stateDir(t)
	if err := restart(t, dir).Close(); err != nil {
		t.Fatal(err)
	}
	back := func() time.Time { return time.Now().Add(-48 * time.Hour) }
	if err := renewBootstrap(context.Background(), Config{StateDir: dir, clock: back}); err != nil {
		t.Fatalf("renew on a clock behind the log: %v", err)
	}
	st, _ := bootRecord(t, dir)
	if at, _ := lastOf(t, dir, identity.BootstrapRenewedKind); st.Secret == nil || string(st.Secret.CreatedAt) != at || !st.Live(time.Now()) {
		t.Errorf("the renewed secret %+v, recorded at %s", st.Secret, at)
	}

	p := recoveryStore(t)
	fixed := time.Now().Add(time.Hour).UTC()
	out := outFile{path: filepath.Join(privateDir(t), "token")}
	if err := recoverToken(context.Background(), Config{StateDir: p.dir, clock: func() time.Time { return fixed }}, p.op, out, false); err != nil {
		t.Fatal(err)
	}
	want := `"created_at":"` + fixed.Format(time.RFC3339Nano) + `"`
	if _, payload := lastOf(t, p.dir, identity.TokenRecoveredKind); !bytes.Contains(payload, []byte(want)) {
		t.Errorf("the recovered token %s, want %s", payload, want)
	}
}
