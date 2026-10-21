// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
)

// chat posts a chat completion with token over client, returning the
// status and the gateway's refusal header.
func chat(t *testing.T, h func(*http.Request) (*http.Response, error), token string) (int, string, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, gateway.BaseURL+"/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get(gateway.RefusalHeader), nil
}

// P22a-5a (W16; #226 entries 57, 58, 78): adcd's gateway serves an
// attempt's own socket and mints the adapter's value, a base_url with no
// path and the binding's token; the attempt's end unbinds the token (a
// request with it is then refused unbound), closes the socket and, the
// call never intended, settles nothing; a second end returns the first's.
func TestEgressAttemptSession(t *testing.T) {
	d := restart(t, stateDir(t))
	t.Cleanup(func() { _ = d.Close() })
	e, err := newEgress(d, registry.Registry{}, false)
	if err != nil {
		t.Fatal(err)
	}
	b := gateway.Binding{Lease: types.LeaseID(newID(t, "lea")), Attempt: types.AttemptID(newID(t, "att")), Call: types.CallID(newID(t, "cal"))}
	socket := filepath.Join(shortDir(t), "gw.sock")
	v, end, err := e.open(b, socket)
	if err != nil || v.BaseUrl != gateway.BaseURL || v.Token == "" {
		t.Fatalf("open: %+v, %v", v, err)
	}
	over := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	if status, refusal, err := chat(t, over.Do, v.Token); err != nil || status != http.StatusConflict || refusal != "lease_not_running_attempt" {
		t.Fatalf("a request on the attempt's socket: %d %q %v", status, refusal, err)
	}
	ctx := context.Background()
	obs, err := end(ctx)
	if err != nil {
		t.Fatalf("the end: %v", err)
	}
	if _, _, err := chat(t, over.Do, v.Token); err == nil {
		t.Error("the socket still serves after the end")
	}
	direct := func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		e.gw.ServeHTTP(deadlined{w}, r)
		return w.Result(), nil
	}
	if status, refusal, _ := chat(t, direct, v.Token); status != http.StatusUnauthorized || refusal == "lease_not_running_attempt" {
		t.Errorf("the token after the end: %d %q", status, refusal)
	}
	if again, err := end(ctx); err != nil || len(again.Statuses) != len(obs.Statuses) {
		t.Errorf("a second end: %+v, %v", again, err)
	}
}

// P22a-5a (§8 item 4): an exit run's capture keeps the body in the capture
// store, never the controller's, and records it (blob.capture_kept: the
// call, restricted, expiring 7 days later). The purge in adcd's loop, run
// at a time past the expiry, removes the bytes and records
// blob.capture_purged once; a second run purges nothing more. A digest
// another live capture holds keeps its bytes until that one's purge.
func TestEgressCaptureAndPurge(t *testing.T) {
	dir := stateDir(t)
	d := restart(t, dir)
	t.Cleanup(func() { _ = d.Close() })
	b := gateway.Binding{Lease: types.LeaseID(newID(t, "lea")), Attempt: types.AttemptID(newID(t, "att")), Call: types.CallID(newID(t, "cal"))}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"canary"}]}`)
	ctx := context.Background()
	if err := captureOf(d).Keep(ctx, b, body); err != nil {
		t.Fatal(err)
	}
	later := b
	later.Call = types.CallID(newID(t, "cal"))
	clock := time.Now()
	d.clock = func() time.Time { return clock.Add(3 * 24 * time.Hour) }
	if err := captureOf(d).Keep(ctx, later, body); err != nil { // the same bytes, kept 3 days later
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if has, err := d.captures.Has(digest); err != nil || !has {
		t.Fatalf("the capture store: %v %v", has, err)
	}
	if has, _ := d.blobs.Has(digest); has {
		t.Error("the capture is in the controller's blob store")
	}
	var kept []types.EgressCapture
	if err := d.store.View(ctx, func(tx *eventstore.Tx) error {
		keys, err := tx.Keys(api.CaptureProjection, "")
		for _, k := range keys {
			raw, _, _ := tx.Get(api.CaptureProjection, k)
			var c struct{ Record types.EgressCapture }
			_ = json.Unmarshal(raw, &c)
			kept = append(kept, c.Record)
		}
		return err
	}); err != nil || len(kept) != 2 || kept[0].Class != gateway.ClassRestricted {
		t.Fatalf("the captures kept: %+v, %v", kept, err)
	}
	purge := func(at time.Time) int {
		t.Helper()
		n, err := api.PurgeCaptures(ctx, d.store, d.captures, d.system, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := purge(clock.Add(8 * 24 * time.Hour)); n != 1 {
		t.Fatalf("the first expiry purged %d", n)
	}
	if has, _ := d.captures.Has(digest); !has {
		t.Error("the bytes the later capture holds are gone")
	}
	if n := purge(clock.Add(11 * 24 * time.Hour)); n != 1 {
		t.Fatalf("the second expiry purged %d", n)
	}
	if has, _ := d.captures.Has(digest); has {
		t.Error("the bytes outlive every capture")
	}
	if n := purge(clock.Add(12 * 24 * time.Hour)); n != 0 {
		t.Errorf("a later run purged %d more", n)
	}
	if n := count(t, dir, `SELECT count(*) FROM events WHERE kind = '`+api.CapturePurged+`'`); n != 2 {
		t.Errorf("purge events: %d", n)
	}
}

// P22a-5a: adcd's purge loop purges expired captures: a capture seeded
// expired before the start is purged by the loop's first run (its bytes
// gone, one blob.capture_purged).
func TestCapturesPurgedByTheLoop(t *testing.T) {
	body := []byte("a request body")
	var digest string
	c := runCommanded(t, func(w *seeder, _ types.ActorID) {
		captures, err := blobstore.Open(filepath.Join(w.dir, "captures"))
		if err == nil {
			digest, err = captures.Put(bytes.NewReader(body), "")
		}
		if err != nil {
			t.Fatal(err)
		}
		r := types.EgressCapture{Digest: types.Digest(digest), Lease: types.LeaseID(newID(t, "lea")), Attempt: types.AttemptID(newID(t, "att")),
			Call: types.CallID(newID(t, "cal")), Class: gateway.ClassRestricted, ExpiresAt: types.Time(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))}
		raw, _ := json.Marshal(r)
		evt := newID(t, "evt")
		w.do(w.sys, func(*eventstore.Tx) ([]eventstore.Event, error) {
			return []eventstore.Event{{ID: evt, Kind: api.CaptureKept, Subject: evt, Payload: raw}}, nil
		})
	})
	for deadline := time.Now().Add(10 * time.Second); count(t, c.dir, `SELECT count(*) FROM events WHERE kind = '`+api.CapturePurged+`'`) != 1; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the loop purged no capture")
		}
	}
	if has, err := c.d.captures.Has(digest); err != nil || has {
		t.Errorf("the purged capture's bytes: %v %v", has, err)
	}
}

// Opus #322 M2: an end called with a cancelled context (a stop, a
// shutdown) still settles an intended call: the settlement runs under a
// bounded context of its own, so the restart's uncertain effect is
// reconciled (its observation moves); a later end finds it settled.
func TestEgressEndSettlesUnderACancelledContext(t *testing.T) {
	_, cfg, a, l, call := cutStore(t)
	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	before, err := d.store.LoadEffect(context.Background(), modelcall.EffectOf(call))
	if err != nil {
		t.Fatal(err)
	}
	e, err := newEgress(d, registry.Registry{}, false)
	if err != nil {
		t.Fatal(err)
	}
	_, end, err := e.open(gateway.Binding{Lease: l, Attempt: a, Call: call}, filepath.Join(shortDir(t), "gw.sock"))
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := end(cancelled); err != nil {
		t.Fatalf("an end under a cancelled context: %v", err)
	}
	after, err := d.store.LoadEffect(context.Background(), modelcall.EffectOf(call))
	if err != nil || after.LastObservation == before.LastObservation {
		t.Fatalf("the call was not settled: %q → %q, %v", before.LastObservation, after.LastObservation, err)
	}
	if _, err := end(cancelled); err != nil {
		t.Errorf("a later end: %v", err)
	}
}

// Opus #322 M1: a capture kept under a cancelled context is still recorded;
// bytes of the capture store that no kept capture names (a record that
// never committed) are swept once older than the grace, never before.
func TestEgressCaptureOutlivesItsRequest(t *testing.T) {
	d := restart(t, stateDir(t))
	t.Cleanup(func() { _ = d.Close() })
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	b := gateway.Binding{Lease: types.LeaseID(newID(t, "lea")), Attempt: types.AttemptID(newID(t, "att")), Call: types.CallID(newID(t, "cal"))}
	if err := captureOf(d).Keep(cancelled, b, []byte("a body whose request ended")); err != nil {
		t.Fatalf("a capture under a cancelled context: %v", err)
	}
	if n := count(t, d.lock.Dir(), `SELECT count(*) FROM events WHERE kind = '`+api.CaptureKept+`'`); n != 1 {
		t.Fatalf("captures recorded: %d", n)
	}
	orphan, err := d.captures.Put(bytes.NewReader([]byte("bytes no record names")), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := api.PurgeCaptures(ctx, d.store, d.captures, d.system, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if has, _ := d.captures.Has(orphan); !has {
		t.Fatal("bytes within the grace were swept")
	}
	if _, err := api.PurgeCaptures(ctx, d.store, d.captures, d.system, time.Now().Add(api.CaptureGrace+time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	if has, _ := d.captures.Has(orphan); has {
		t.Error("bytes no record names outlived the grace")
	}
	kept := blobDigest([]byte("a body whose request ended"))
	if has, _ := d.captures.Has(kept); !has {
		t.Error("a kept capture's bytes were swept")
	}
}

// Mythos #359 L1: a capture whose record the store does not take is
// bounded: the record runs under a deadline (captureRecordBound), even for a
// request whose context ended, and its failure fails the capture, so the
// gateway refuses capture_failed and the lock is free again.
func TestEgressCaptureRecordIsBounded(t *testing.T) {
	d := restart(t, stateDir(t))
	t.Cleanup(func() { _ = d.Close() })
	var bound time.Duration
	recordCapture = func(ctx context.Context, _ *eventstore.Store, _ identity.Principal, _ gateway.Captured) error {
		dl, ok := ctx.Deadline()
		if ok {
			bound = time.Until(dl)
		}
		return context.DeadlineExceeded // the writer did not answer in time
	}
	t.Cleanup(func() { recordCapture = api.RecordCapture })
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	b := gateway.Binding{Lease: types.LeaseID(newID(t, "lea")), Attempt: types.AttemptID(newID(t, "att")), Call: types.CallID(newID(t, "cal"))}
	if err := captureOf(d).Keep(cancelled, b, []byte("a body")); err == nil {
		t.Fatal("a capture whose record timed out was kept")
	}
	if bound <= 0 || bound > captureRecordBound {
		t.Errorf("the record's deadline: %s from now, want within %s", bound, captureRecordBound)
	}
	if !d.capturing.TryLock() {
		t.Fatal("the capture lock is still held")
	}
	d.capturing.Unlock()
}

// blobDigest is b's blob digest.
func blobDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Opus #322 L1: a settlement that fails is retried by a later end: the
// first end returns its error and leaves the call as it was; the second
// settles it; a third settles nothing more.
func TestEgressEndRetriesAFailedSettlement(t *testing.T) {
	_, cfg, a, l, call := cutStore(t)
	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	calls := 0
	settleCall = func(e *egress, ctx context.Context, call types.CallID) error {
		if calls++; calls == 1 {
			return errors.New("the settlement failed (the test)")
		}
		return e.settle(ctx, call)
	}
	t.Cleanup(func() { settleCall = (*egress).settle })
	effect := func() string {
		eff, err := d.store.LoadEffect(context.Background(), modelcall.EffectOf(call))
		if err != nil {
			t.Fatal(err)
		}
		return eff.LastObservation
	}
	before := effect()
	e, err := newEgress(d, registry.Registry{}, false)
	if err != nil {
		t.Fatal(err)
	}
	_, end, err := e.open(gateway.Binding{Lease: l, Attempt: a, Call: call}, filepath.Join(shortDir(t), "gw.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := end(ctx); err == nil || effect() != before {
		t.Fatalf("the first end: %v; the observation %q → %q", err, before, effect())
	}
	if _, err := end(ctx); err != nil || effect() == before {
		t.Fatalf("the second end: %v; the observation %q → %q", err, before, effect())
	}
	if _, err := end(ctx); err != nil || calls != 2 {
		t.Errorf("a third end: %v, %d settlements", err, calls)
	}
}

// Opus #322 L2: a capture waits for the lock the purge holds: it neither
// stores nor records while held, and does once released.
func TestEgressCaptureWaitsForThePurge(t *testing.T) {
	dir := stateDir(t)
	d := restart(t, dir)
	t.Cleanup(func() { _ = d.Close() })
	b := gateway.Binding{Lease: types.LeaseID(newID(t, "lea")), Attempt: types.AttemptID(newID(t, "att")), Call: types.CallID(newID(t, "cal"))}
	kept := func() int { return count(t, dir, `SELECT count(*) FROM events WHERE kind = '`+api.CaptureKept+`'`) }
	stored := func() int {
		bs, err := d.captures.Stored()
		if err != nil {
			t.Fatal(err)
		}
		return len(bs)
	}
	d.capturing.Lock()
	done := make(chan error, 1)
	go func() { done <- captureOf(d).Keep(context.Background(), b, []byte("a body")) }()
	time.Sleep(200 * time.Millisecond)
	if kept() != 0 || stored() != 0 {
		d.capturing.Unlock()
		t.Fatal("a capture went past the purge's lock")
	}
	d.capturing.Unlock()
	if err := <-done; err != nil || kept() != 1 || stored() != 1 {
		t.Fatalf("the capture once released: %v, %d kept, %d stored", err, kept(), stored())
	}
}

// deadlined is a recorder that takes the write deadline the gateway arms
// (#308: a writer that cannot take one aborts the request).
type deadlined struct{ *httptest.ResponseRecorder }

func (deadlined) SetWriteDeadline(time.Time) error { return nil }
