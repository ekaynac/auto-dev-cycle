// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"syscall"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// StartError is a refused start: a registered outcome and reason with a
// fixed-text detail. Error never includes the cause, which may name paths
// or stored values; Unwrap keeps it for errors.Is.
type StartError struct {
	Outcome vocab.Outcome
	Reason  vocab.Reason
	Detail  string
	cause   error
}

func (e *StartError) Error() string {
	return "adcd: " + string(e.Outcome) + "/" + e.Reason.String() + ": " + e.Detail
}

func (e *StartError) Unwrap() error { return e.cause }

func couldNotRun(reason vocab.Reason, detail string, cause error) error {
	return &StartError{Outcome: vocab.CouldNotRun, Reason: reason, Detail: detail, cause: cause}
}

// RunCommand is `adcd run`: it runs the daemon until SIGINT or SIGTERM and
// returns the exit code of the single exit-code table (08), printing fixed
// text only.
func RunCommand(args []string, stderr io.Writer) int {
	cfg, err := LoadConfig(args)
	if err == nil {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		err = Run(ctx, cfg, stderr, nil)
	}
	return exitCode(err, stderr)
}

// exitCode prints err's fixed text and returns its exit code (08).
func exitCode(err error, stderr io.Writer) int {
	var se *StartError
	switch {
	case err == nil:
		return entry.ExitOK
	case errors.Is(err, ErrConfig):
		fmt.Fprintln(stderr, err)
		return entry.ExitUsage
	case errors.As(err, &se) && se.Outcome == vocab.Failed:
		fmt.Fprintln(stderr, se.Error())
		return entry.ExitRefused
	case errors.As(err, &se):
		fmt.Fprintln(stderr, se.Error())
		return entry.ExitCouldNotRun
	}
	fmt.Fprintln(stderr, "adcd: could_not_run: the daemon stopped on an unexpected error")
	return entry.ExitCouldNotRun
}
