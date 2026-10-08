// SPDX-License-Identifier: Apache-2.0

//go:build linux

package adapterhost

import (
	"context"
	"errors"
	"time"
)

// Bounds are an attempt's waits on its adapter: Start, from the spawn to
// describe's reply; Handshake, prepare's reply; Result, run's result;
// Shutdown, shutdown's reply and the process tree's exit after it, before
// the namespace is killed. A zero Start, Handshake or Result leaves that
// wait to the caller's context alone; a zero Shutdown is shutdownTimeout.
// The session's Deadline still bounds the whole process (the backstop).
type Bounds struct {
	Start, Handshake, Result, Shutdown time.Duration
}

// ShutdownBound is the shutdown wait's bound: Shutdown, or shutdownTimeout.
func (b Bounds) ShutdownBound() time.Duration {
	if b.Shutdown <= 0 {
		return shutdownTimeout
	}
	return b.Shutdown
}

// TimeoutError is a wait of Bounds that passed its bound (the caller's own
// context still live). Its text is fixed; it is a context.DeadlineExceeded.
type TimeoutError struct {
	Wait string // start, handshake, result or shutdown
}

func (e *TimeoutError) Error() string {
	return "adapterhost: the adapter's " + e.Wait + " passed its bound"
}

func (e *TimeoutError) Unwrap() error { return context.DeadlineExceeded }

// bounded runs call under bound d (ctx alone when d is zero). A call that
// failed because d passed, while ctx is still live, is a *TimeoutError
// naming wait.
func bounded[T any](ctx context.Context, d time.Duration, wait string, call func(context.Context) (T, error)) (T, error) {
	if d <= 0 {
		return call(ctx)
	}
	bctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	v, err := call(bctx)
	if err != nil && ctx.Err() == nil && errors.Is(bctx.Err(), context.DeadlineExceeded) {
		return v, &TimeoutError{Wait: wait}
	}
	return v, err
}
