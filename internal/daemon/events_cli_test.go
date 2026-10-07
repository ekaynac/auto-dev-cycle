// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/cli"
	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

// lockedBuffer is a buffer a running command writes while a test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// 14 PR 10b against the daemon: `adc events --follow --json` prints the
// events a new grant reads, one per line in seq order, and ends with exit 1
// when the grant is revoked; no token is printed.
func TestEventsThroughTheCLI(t *testing.T) {
	c := runCommanded(t)
	op, bare := c.op.Reveal(), c.bare.Reveal()
	var outs []string
	adc := func(tok string, args ...string) (int, string) {
		t.Helper()
		code, out, errb := c.adc(tok, args...)
		outs = append(outs, out, errb)
		return code, out
	}

	grant := newID(t, "grt")
	create := `{"id":"` + grant + `","actor":"` + string(c.bareID) + `","projects":"*","actions":[],"reads":{"projects":"same","kinds":["grant","note"]},"expires_at":"` +
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `","confirmation_overrides":{}}`
	code, out := adc(op, "grant", "create", "--expect", "0", "--reason", "r", "--json", "--scope", create)
	created, ok := response(t, out).(*types.CommandApplied)
	if code != entry.ExitOK || !ok {
		t.Fatalf("grant: %d %s", code, out)
	}
	env := map[string]string{cli.SocketEnv: filepath.Join(c.dir, privateSocket), client.TokenEnv: bare}
	var stdout, stderr lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- cli.Main(context.Background(), []string{"events", "--follow", "--json"}, &stdout, &stderr, cli.ClientFromEnv(func(k string) string { return env[k] }))
	}()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(stdout.String(), grant); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the grant's event never came: %s %s", stdout.String(), stderr.String())
		}
	}
	if code, out := adc(op, "grant", "revoke", "--expect", strconv.FormatInt(created.LastSeq, 10), "--reason", "r", "--scope", `{"grant":"`+grant+`"}`); code != entry.ExitOK {
		t.Fatalf("revoke: %d %s", code, out)
	}
	select {
	case code := <-done:
		if code != entry.ExitRefused || !strings.Contains(stderr.String(), "closed: failed/authority_revoked") {
			t.Fatalf("closed: %d %s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stream did not close on revocation")
	}
	last := int64(0)
	for _, line := range strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n") {
		var e struct{ Seq int64 }
		if json.Unmarshal([]byte(line), &e) != nil || e.Seq <= last {
			t.Fatalf("not one event per line in seq order: %q", line)
		}
		last = e.Seq
	}
	secretNotPrinted(t, append(outs, stdout.String(), stderr.String()), c.op, c.bare)
}
