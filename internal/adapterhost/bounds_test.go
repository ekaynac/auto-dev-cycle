// SPDX-License-Identifier: Apache-2.0

//go:build linux

package adapterhost

import (
	"context"
	"errors"
	"testing"
	"time"
)

// bounded names its wait only when its own bound passed: the caller's
// cancellation, or no bound, is never a *TimeoutError.
func TestBounded(t *testing.T) {
	wait := func(ctx context.Context) (int, error) { <-ctx.Done(); return 0, ctx.Err() }
	var te *TimeoutError
	if _, err := bounded(context.Background(), 20*time.Millisecond, "result", wait); !errors.As(err, &te) || te.Wait != "result" ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("own bound: %v", err)
	}
	if _, err := bounded(context.Background(), time.Hour, "result", func(context.Context) (int, error) { return 0, errors.New("protocol") }); errors.As(err, &te) {
		t.Fatalf("another error: %v", err)
	}
	parent, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	if _, err := bounded(parent, time.Hour, "result", wait); errors.As(err, &te) || !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller's cancellation: %v", err)
	}
	expired, cancel2 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel2()
	if _, err := bounded(expired, time.Hour, "result", wait); errors.As(err, &te) {
		t.Fatalf("the caller's own deadline: %v", err)
	}
	if _, err := bounded(context.Background(), 0, "result", func(ctx context.Context) (int, error) {
		if _, ok := ctx.Deadline(); ok {
			return 0, errors.New("a zero bound set a deadline")
		}
		return 0, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A zero Shutdown is 5 s; another is itself.
func TestShutdownBound(t *testing.T) {
	if d := (Bounds{}).ShutdownBound(); d != 5*time.Second {
		t.Fatalf("zero: %s", d)
	}
	if d := (Bounds{Shutdown: time.Second}).ShutdownBound(); d != time.Second {
		t.Fatalf("set: %s", d)
	}
}
