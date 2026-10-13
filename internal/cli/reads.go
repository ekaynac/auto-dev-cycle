// SPDX-License-Identifier: Apache-2.0

package cli

// The read commands (08 "Reads"): one per resource the controller
// registers, and the caller's own whoami. --json prints the controller's
// body as is, once the transport has validated it; human output escapes
// every string (render.go). Holds wait for their read (14 9e; M1 P4a).

import (
	"context"
	"errors"
	"flag"
	"io"

	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Reader is a Client that also reads resources.
type Reader interface {
	Read(ctx context.Context, resource string) ([]byte, error)
}

// reads maps each read command to its resource.
var reads = map[command]string{
	{"grant", "list"}:   "grants",
	{"propose", "list"}: "proposals",
	{"seat", "status"}:  "seat",
	{"notes", "list"}:   "notes",
	{"project", "list"}: "projects",
	{"admin", "whoami"}: client.Whoami,
}

func read(ctx context.Context, name, resource string, args []string, stdout, stderr io.Writer, c Client) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the controller's response as one JSON document")
	if code, ok := parse(fs, args, stderr); !ok {
		return code
	}
	if fs.NArg() > 0 {
		say(stderr, "%s: unexpected argument %s\n", name, fs.Arg(0))
		return entry.ExitUsage
	}
	r, ok := c.(Reader)
	if !ok {
		return failed(stderr, name, ErrNoTransport)
	}
	raw, err := r.Read(ctx, resource)
	if err != nil {
		return failed(stderr, name, err)
	}
	if *asJSON {
		err = sayJSON(stdout, raw)
	} else {
		err = render(stdout, resource, raw)
	}
	if err != nil {
		say(stderr, "%s: could_not_run: writing the result: %s\n", name, err)
		return entry.ExitCouldNotRun
	}
	return entry.ExitOK
}

// failed reports why a read or the stream did not run, with its exit
// code; nothing it prints is the controller's text unescaped.
func failed(stderr io.Writer, name string, err error) int {
	var refused *client.Refused
	switch {
	case errors.As(err, &refused):
		say(stderr, "%s: %s/%s: %s\n", name, refused.Result.Outcome, refused.Result.Reason, refused.Result.Detail)
		return resultCode(refused.Result)
	case errors.Is(err, client.ErrUnauthenticated):
		say(stderr, "%s: refused: %s\n", name, err)
		return entry.ExitRefused
	case errors.Is(err, client.ErrBadRequest):
		say(stderr, "%s: %s\n", name, err)
		return entry.ExitUsage
	}
	say(stderr, "%s: could_not_run: %s\n", name, err)
	return entry.ExitCouldNotRun
}

// resultCode is a refusal's or a closed stream's exit code.
func resultCode(r vocab.Result) int {
	if r.Outcome == vocab.CouldNotRun {
		return entry.ExitCouldNotRun
	}
	return entry.ExitRefused
}
