// SPDX-License-Identifier: Apache-2.0

package identity_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

func (f *fixture) principal(bearer string, via identity.Listener) (identity.Principal, error) {
	var p identity.Principal
	var authErr error
	err := f.s.View(context.Background(), func(tx *eventstore.Tx) error {
		p, authErr = identity.Authenticated(tx, bearer, via)
		return nil
	})
	return p, errors.Join(err, authErr)
}

// A principal comes from a live bearer on a listener the server names: a
// missing or malformed bearer, or no listener, gives none.
func TestAuthenticatedNeedsBearerAndListener(t *testing.T) {
	f := open(t)
	for _, via := range []identity.Listener{identity.PrivateSocket, identity.SharedSocket, identity.HTTPS} {
		p, err := f.principal(f.token.Reveal(), via)
		if err != nil || p.Actor() != f.op.ID || p.Via() != via {
			t.Fatalf("the operator's bearer on %d: %+v %v", via, p, err)
		}
	}
	for name, c := range map[string]struct {
		bearer string
		via    identity.Listener
	}{
		"no bearer":      {"", identity.PrivateSocket},
		"a bad bearer":   {"tok_x.y", identity.PrivateSocket},
		"no listener":    {f.token.Reveal(), 0},
		"a bad listener": {f.token.Reveal(), identity.HTTPS + 1},
	} {
		if _, err := f.principal(c.bearer, c.via); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var zero identity.Principal
	if err := f.s.View(context.Background(), func(tx *eventstore.Tx) error {
		return identity.Recheck(tx, zero.Command(id(t, "cmd"), "note.add", "k", []byte(`{}`), nil))
	}); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("the zero principal passed the recheck: %v", err)
	}
}

// No token yields the system principal, even a token record forged for
// the system actor that authenticates; SystemPrincipal makes it only for the
// live system actor.
func TestNoSystemPrincipalFromAToken(t *testing.T) {
	f := open(t)
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	tok := types.Token{ID: types.TokenID(id(t, "tok")), Actor: f.sys.ID, Hash: identity.Hash(secret, 3, 64*1024, 4),
		CreatedBy: f.sys.ID, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		rec, err := json.Marshal(tok)
		if err == nil {
			err = tx.Put(identity.TokenProjection, string(tok.ID), rec)
		}
		return nil, err
	})
	bearer := string(tok.ID) + "." + secret
	if a, err := f.authenticate(bearer); err != nil || a.ID != f.sys.ID {
		t.Fatalf("the forged record does not authenticate, so the test proves nothing: %v", err)
	}
	for _, via := range []identity.Listener{identity.PrivateSocket, identity.SharedSocket, identity.HTTPS} {
		if p, err := f.principal(bearer, via); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Fatalf("a token gave the system principal on %d: %+v %v", via, p, err)
		}
	}
	err := f.s.View(context.Background(), func(tx *eventstore.Tx) error {
		if _, err := identity.SystemPrincipal(tx, f.op.ID); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Errorf("an operator's system principal: %v", err)
		}
		p, err := identity.SystemPrincipal(tx, f.sys.ID)
		if err != nil {
			return err
		}
		// Its commands are the system actor's, and nobody else's.
		c := p.Command(id(t, "cmd"), "note.add", "k", []byte(`{}`), nil)
		if err := identity.Recheck(tx, c); err != nil || c.Actor != string(f.sys.ID) {
			t.Errorf("the system principal's own command: %v", err)
		}
		c.Actor = string(f.op.ID)
		if err := identity.Recheck(tx, c); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Errorf("the system principal acting as the operator: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// systemPrincipalCallers are the only packages whose non-test files may
// call identity.SystemPrincipal: the daemon's private startup path.
var systemPrincipalCallers = map[string]bool{"internal/daemon": true}

// Only the daemon's startup holds the system principal: every non-test
// .go file of the module is parsed for a reference to SystemPrincipal.
func TestSystemPrincipalIsTheDaemonsOnly(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); path != root && (strings.HasPrefix(n, ".") || n == "node_modules" || n == "testdata" || n == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "SystemPrincipal" {
				found++
				if !systemPrincipalCallers[filepath.ToSlash(rel)] {
					t.Errorf("%s references SystemPrincipal", filepath.ToSlash(rel))
				}
			}
			return true
		})
		return nil
	})
	if err != nil || found == 0 {
		t.Fatalf("the walk found %d references: %v", found, err)
	}
}
