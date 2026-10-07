// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
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

// whose reports which actor each bearer authenticates as ("" when it
// does not), read offline.
func whose(t *testing.T, dir string, bearers ...string) []types.ActorID {
	t.Helper()
	d, err := start(context.Background(), Config{StateDir: dir}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	out := make([]types.ActorID, len(bearers))
	if err := d.store.View(context.Background(), func(tx *eventstore.Tx) error {
		for i, b := range bearers {
			if a, err := identity.Authenticate(tx, b); err == nil {
				out[i] = a.ID
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func readToken(t *testing.T, p string) string {
	t.Helper()
	if st := lstat(t, p); st.Mode&0o170777 != 0o100600 || int(st.Uid) != os.Geteuid() {
		t.Fatalf("--out mode %o uid %d", st.Mode, st.Uid)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// Recovery issues the operator a working token, written only to a 0600
// --out; it refuses an existing --out unless --force-overwrite, and
// --revoke-others revokes exactly that operator's other live tokens.
func TestRecoverOperatorToken(t *testing.T) {
	p := recoveryStore(t)
	out := filepath.Join(privateDir(t), "token")
	args := []string{"--state-dir", p.dir, "--actor", string(p.op), "--out", out}
	code, stderr := command(RecoverCommand, args...)
	if code != entry.ExitOK {
		t.Fatalf("recover: %d %s", code, stderr)
	}
	first := readToken(t, out)
	if got := whose(t, p.dir, first, p.opTokens[0], p.opTokens[1]); got[0] != p.op || got[1] != p.op || got[2] != p.op {
		t.Fatalf("after recovery: %v", got)
	}
	seq := lastSeq(t, p.dir)
	if code, msg := command(RecoverCommand, args...); code != entry.ExitUsage || !strings.Contains(msg, "--out exists") || lastSeq(t, p.dir) != seq {
		t.Fatalf("an existing --out: %d %s", code, msg)
	}
	code, stderr2 := command(RecoverCommand, append(args, "--force-overwrite", "--revoke-others")...)
	if code != entry.ExitOK {
		t.Fatalf("recover again: %d %s", code, stderr2)
	}
	second := readToken(t, out)
	got := whose(t, p.dir, second, first, p.opTokens[0], p.opTokens[1], p.otherToken, p.supToken)
	if want := []types.ActorID{p.op, "", "", "", p.other, p.sup}; !equal(got, want) {
		t.Fatalf("after --revoke-others: %v, want %v", got, want)
	}
	noSecret(t, p.dir, []string{stderr, stderr2}, secretPart(t, first), secretPart(t, second))
}

func equal(a, b []types.ActorID) bool {
	return strings.Join(ids2s(a), ",") == strings.Join(ids2s(b), ",")
}

func ids2s(a []types.ActorID) []string {
	s := make([]string, len(a))
	for i, v := range a {
		s[i] = string(v)
	}
	return s
}

func secretPart(t *testing.T, bearer string) string {
	t.Helper()
	_, s, ok := strings.Cut(bearer, ".")
	if !ok || s == "" {
		t.Fatal("a bearer without its secret part")
	}
	return s
}

// A target that is missing, revoked or not an operator is refused, and
// nothing is recorded or written.
func TestRecoverTargetRefused(t *testing.T) {
	p := recoveryStore(t)
	seq := lastSeq(t, p.dir)
	for name, target := range map[string]types.ActorID{"missing": types.ActorID(newID(t, "act")), "revoked": p.rv, "supervisor": p.sup, "system": p.sys} {
		out := filepath.Join(privateDir(t), "token")
		code, msg := command(RecoverCommand, "--state-dir", p.dir, "--actor", string(target), "--out", out)
		if code != entry.ExitRefused || !strings.Contains(msg, "failed/recipient_not_operator: the target is not a live operator") {
			t.Errorf("%s: %d %s", name, code, msg)
		}
		gone(t, out)
	}
	if lastSeq(t, p.dir) != seq {
		t.Fatal("a refused recovery appended to the log")
	}
	lockFree(t, p.dir)
}

// The --out directory must be this user's and closed to group and other
// writes, and is never reached through a symlink.
func TestRecoverOutRefused(t *testing.T) {
	p := recoveryStore(t)
	open := privateDir(t)
	if err := os.Chmod(open, 0o770); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(privateDir(t), link); err != nil {
		t.Fatal(err)
	}
	seq := lastSeq(t, p.dir)
	for _, out := range []string{filepath.Join(open, "token"), filepath.Join(link, "token")} {
		code, msg := command(RecoverCommand, "--state-dir", p.dir, "--actor", string(p.op), "--out", out)
		if code != entry.ExitUsage || !strings.Contains(msg, "--out directory") {
			t.Errorf("%d %s", code, msg)
		}
	}
	if lastSeq(t, p.dir) != seq {
		t.Fatal("a refused recovery appended to the log")
	}
	if code, msg := command(RecoverCommand, "--state-dir", p.dir); code != entry.ExitUsage || !strings.Contains(msg, "--actor and --out are required") {
		t.Errorf("no --actor: %d %s", code, msg)
	}
}

// --force-overwrite waives only the refusal of an existing --out: a
// directory, or an --out that cannot be inspected, is refused before the
// store opens, with nothing recorded or revoked and the destination left
// as it was.
func TestRecoverForcedOutRefused(t *testing.T) {
	p := recoveryStore(t)
	dir := privateDir(t)
	sub := filepath.Join(dir, "token")
	mkdir(t, sub)
	seq := lastSeq(t, p.dir)
	before := tree(t, dir)
	for out, want := range map[string]string{
		sub: "--out is not a regular file or a symlink",
		filepath.Join(dir, strings.Repeat("x", 300)): "--out cannot be inspected",
	} {
		code, msg := command(RecoverCommand, "--state-dir", p.dir, "--actor", string(p.op), "--out", out, "--force-overwrite", "--revoke-others")
		if code != entry.ExitUsage || !strings.Contains(msg, want) {
			t.Errorf("%d %s", code, msg)
		}
	}
	if got := whose(t, p.dir, p.opTokens...); got[0] != p.op || got[1] != p.op || lastSeq(t, p.dir) != seq {
		t.Fatalf("a refused recovery changed the tokens: %v", got)
	}
	if after := tree(t, dir); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("the destination changed: %v", after)
	}
}

// The rename never replaces a file that appears after the check: the
// sentinel survives and no temporary file is left.
func TestRecoverWriteNoReplace(t *testing.T) {
	dir := privateDir(t)
	o := outFile{path: filepath.Join(dir, "token")}
	d, err := o.open()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	writeFile0600(t, o.path, "sentinel")
	_, secret, err := identity.IssueToken(types.ActorID(newID(t, "act")), types.ActorID(newID(t, "act")), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.write(d, secret); err == nil {
		t.Fatal("the write replaced an existing file")
	}
	if b, err := os.ReadFile(o.path); err != nil || string(b) != "sentinel" {
		t.Fatalf("the sentinel is gone: %q %v", b, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Fatalf("a temporary file is left: %v %v", entries, err)
	}
}

// A token recorded but not delivered (the --out directory refuses the
// write) is reported with fixed text; a retry with --revoke-others
// retires it and delivers a working one.
func TestRecoverDeliveryFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a 0500 directory")
	}
	p := recoveryStore(t)
	dir := privateDir(t)
	out := filepath.Join(dir, "token")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	seq := lastSeq(t, p.dir)
	code, msg := command(RecoverCommand, "--state-dir", p.dir, "--actor", string(p.op), "--out", out)
	if code != entry.ExitCouldNotRun || msg != "adcd: could_not_run/store_unreadable: the token is recorded but the --out file could not be written; recover again with --revoke-others\n" {
		t.Fatalf("%d %q", code, msg)
	}
	if lastSeq(t, p.dir) != seq+1 {
		t.Fatal("the undelivered token was not recorded")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("the failed write left %v %v", entries, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, msg := command(RecoverCommand, "--state-dir", p.dir, "--actor", string(p.op), "--out", out, "--revoke-others"); code != entry.ExitOK {
		t.Fatalf("retry: %d %s", code, msg)
	}
	bearer := readToken(t, out)
	d, err := start(context.Background(), Config{StateDir: p.dir}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.store.View(context.Background(), func(tx *eventstore.Tx) error {
		live, err := liveTokens(tx, p.op)
		a, aerr := identity.Authenticate(tx, bearer)
		if len(live) != 1 || aerr != nil || a.ID != p.op {
			t.Errorf("after the retry: %d live token(s), %v", len(live), aerr)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
