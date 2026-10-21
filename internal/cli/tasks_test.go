// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

// counting is a Client whose reads answer body, after failing the first
// fails of them, and are counted.
type counting struct {
	fake
	body  string
	fails int64
	reads atomic.Int64
}

func (c *counting) Read(context.Context, string) ([]byte, error) {
	if n := c.reads.Add(1); n <= c.fails {
		return nil, errors.New("the controller is restarting")
	}
	return []byte(c.body), nil
}

const runningTask = `{"resource":"tasks","outcome":"found","items":[{"id":"tsk_01m3250v9xft08ssr4ygq6g2np","project":"prj_01m3250vkwffc979nzjw0g2sa1",` +
	`"card":{"type":"placed","id":"tsk_01m3250v9xft08ssr4ygq6g2np","project":"prj_01m3250vkwffc979nzjw0g2sa1","title":"t","column":"running",` +
	`"admitted":true,"cancelled":false,"abandoned":false,"approval":"in_force","created_at":"2026-10-21T00:00:00Z","nodes":[]},"first_attempt":null}]}`

// P22a-7 (Opus #320 M2, L3): a wait reads once per --every, never in a
// busy loop: over 1 s at 300 ms, at most five reads, then exit 5; a read the
// controller could not answer is retried at the next tick, not the end of
// the wait.
func TestTaskWaitPaces(t *testing.T) {
	c := &counting{body: runningTask, fails: 2}
	code, _, errb := run2(c, "task", "wait", "tsk_01m3250v9xft08ssr4ygq6g2np", "--until", "closed", "--timeout", "1s", "--every", "300ms")
	if n := c.reads.Load(); code != entry.ExitTimedOut || n < 3 || n > 5 {
		t.Fatalf("exit %d after %d reads: %s", code, n, errb)
	}
	start := time.Now()
	if code, _, _ := run2(&counting{body: runningTask}, "task", "wait", "tsk_01m3250v9xft08ssr4ygq6g2np", "--until", "closed", "--timeout", "500ms"); code != entry.ExitTimedOut || time.Since(start) > 3*time.Second {
		t.Fatalf("exit %d after %s", code, time.Since(start))
	}
}
