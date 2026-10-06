// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Without an Identify hook every command is refused, a replay included.
func TestNoIdentityCheckFailsClosed(t *testing.T) {
	e := openWriter(t, Hooks{Authorize: allow})
	if _, err := e.s.SubmitApplied(context.Background(), command(t, "k", `{}`), Applies(appendTo(t, testSubject, 1))); err == nil || !strings.Contains(err.Error(), "identity check") {
		t.Fatalf("a command without an identity check: %v", err)
	}
}

// The identity recheck runs before the idempotency lookup: once it
// refuses, an applied command no longer replays and a reused key is not
// even compared; nothing is written.
func TestIdentifyPrecedesReplay(t *testing.T) {
	errGone := errors.New("principal gone")
	var refuse bool
	e := openWriter(t, Hooks{Authorize: allow, Identify: func(*Tx, Command) error {
		if refuse {
			return errGone
		}
		return nil
	}})
	ctx := context.Background()
	if _, err := e.s.SubmitApplied(ctx, command(t, "k", `{"a": 1}`), Applies(appendTo(t, testSubject, 1))); err != nil {
		t.Fatal(err)
	}
	refuse = true
	for _, input := range []string{`{"a": 1}`, `{"a": 2}`} {
		if _, err := e.s.Submit(ctx, command(t, "k", input), Applies(appendTo(t, testSubject, 1))); !errors.Is(err, errGone) {
			t.Fatalf("%s after the principal went: %v", input, err)
		}
	}
	if h := head(t, e); h != 1 {
		t.Fatalf("head %d after refusals", h)
	}
}
