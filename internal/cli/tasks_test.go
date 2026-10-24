// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// counting is a Client whose reads answer body, after failing the first
// fails of them, and are counted.
type counting struct {
	fake
	body  string
	fails int64
	reads atomic.Int64
}

func (c *counting) ReadTask(context.Context, string) ([]byte, error) {
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

// lost is a Client whose reads answer body twice, then err on every read.
type lost struct {
	fake
	body  string
	err   error
	reads atomic.Int64
}

func (c *lost) ReadTask(context.Context, string) ([]byte, error) {
	if c.reads.Add(1) <= 2 {
		return []byte(c.body), nil
	}
	return nil, c.err
}

// Astra #335 M2: a wait whose token stops authenticating (401) or whose
// last grant goes (403 failed/action_not_granted) mid-wait is refused at
// once, exit 1, never retried to its hour's timeout; so does a controller
// that serves no task read (404, an adcd from before it: exit 2, as adc task
// show; Mythos #353 L1); a could_not_run answer (503 store_unreadable) is
// retried, to the timeout.
func TestTaskWaitRefusalsEndIt(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		want int
	}{
		"the token lost":            {client.ErrUnauthenticated, entry.ExitRefused},
		"the grant lost":            {&client.Refused{Result: vocab.Result{Outcome: vocab.Failed, Reason: vocab.ReasonActionNotGranted}}, entry.ExitRefused},
		"an adcd with no task read": {client.ErrNoResource, entry.ExitCouldNotRun},
		"could_not_run":             {&client.Refused{Result: vocab.Result{Outcome: vocab.CouldNotRun, Reason: vocab.ReasonStoreUnreadable}}, entry.ExitTimedOut},
	} {
		t.Run(name, func(t *testing.T) {
			timeout := "60m"
			if c.want == entry.ExitTimedOut {
				timeout = "700ms"
			}
			r := &lost{body: runningTask, err: c.err}
			type ended struct {
				code int
				errb string
			}
			done := make(chan ended, 1)
			go func() {
				code, _, errb := run2(r, "task", "wait", "tsk_01m3250v9xft08ssr4ygq6g2np", "--until", "closed", "--timeout", timeout, "--every", "100ms")
				done <- ended{code, errb}
			}()
			select {
			case e := <-done:
				if n := r.reads.Load(); e.code != c.want || c.want != entry.ExitTimedOut && n != 3 {
					t.Errorf("exit %d after %d reads: %s", e.code, n, e.errb)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("still waiting after 10s, %d reads", r.reads.Load())
			}
		})
	}
}
