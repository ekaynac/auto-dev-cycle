// SPDX-License-Identifier: Apache-2.0

package cli

// `adc events --follow` (08 "Events"): the event stream from --after,
// one event per line. A closed frame ends it with that result's exit code
// (failed/authority_revoked: 1; could_not_run/store_unreadable: 2) and is
// never resumed. A stream that ends without one (the controller cut a
// stalled client off, or went away) is resumed by seq, as 08 has the client
// do, but only maxResumes times in a row without a new event.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Streamer is a Client that also opens the event stream.
type Streamer interface {
	Follow(ctx context.Context, after int64) (io.ReadCloser, error)
}

const (
	maxFrameBytes = 1 << 20 // one SSE line
	maxResumes    = 3       // resumes in a row that bring no new event
)

// resumeWait is the pause before a resume; tests shorten it.
var resumeWait = time.Second

// errFrame: the stream broke its framing; it is never resumed.
var errFrame = errors.New("the controller's event stream is malformed")

// streamed is one event frame's data, as api.Events sends it.
type streamed struct {
	Seq             int64           `json:"seq"`
	ID              string          `json:"id"`
	At              string          `json:"at"`
	Kind            string          `json:"kind"`
	Subject         string          `json:"subject"`
	Project         *string         `json:"project"`
	Actor           string          `json:"actor"`
	Command         *string         `json:"command"`
	Causation       *string         `json:"causation"`
	SchemaSet       string          `json:"schema_set"`
	Payload         json.RawMessage `json:"payload"`
	PrevHash        string          `json:"prev_hash"`
	PayloadWithheld bool            `json:"payload_withheld"`
}

func events(ctx context.Context, args []string, stdout, stderr io.Writer, c Client) int {
	const name = "adc events"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	follow := fs.Bool("follow", false, "follow the stream (required: it has no end)")
	after := fs.Int64("after", 0, "resume after this seq")
	asJSON := fs.Bool("json", false, "print one JSON event per line")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		return entry.ExitOK
	} else if err != nil {
		return entry.ExitUsage
	}
	if fs.NArg() > 0 || !*follow || *after < 0 {
		say(stderr, "%s: usage: adc events --follow [--after SEQ] [--json]\n", name)
		return entry.ExitUsage
	}
	s, ok := c.(Streamer)
	if !ok {
		return failed(stderr, name, ErrNoTransport)
	}
	last, misses := *after, 0
	for {
		body, err := s.Follow(ctx, last)
		var refused *client.Refused
		if err != nil && (misses == 0 || errors.As(err, &refused) || errors.Is(err, client.ErrUnauthenticated)) {
			return failed(stderr, name, err)
		}
		progressed := false
		if err == nil {
			var closed *vocab.Result
			progressed, closed, err = relay(body, &last, stdout, *asJSON)
			body.Close()
			if closed != nil {
				say(stderr, "%s: closed: %s/%s: %s\n", name, closed.Outcome, closed.Reason, closed.Detail)
				return resultCode(*closed)
			}
			if err != nil {
				return failed(stderr, name, err)
			}
		}
		if progressed {
			misses = 0
		}
		if misses++; misses > maxResumes {
			return failed(stderr, name, fmt.Errorf("the stream ended %d times without a new event; resume with --after %d", maxResumes, last))
		}
		select {
		case <-ctx.Done():
		case <-time.After(resumeWait):
		}
	}
}

// relay copies the stream's event frames to out until it ends: whether a
// new event came, the closing result if one did, and errFrame (or a
// failed write) when it may not be resumed. An event at or before *last
// is malformed: the stream resumes strictly after it.
func relay(body io.Reader, last *int64, out io.Writer, asJSON bool) (bool, *vocab.Result, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 4096), maxFrameBytes)
	progressed := false
	frame := map[string]string{}
	for sc.Scan() {
		line := sc.Text()
		if line != "" {
			k, v, ok := strings.Cut(line, ": ")
			if _, dup := frame[k]; !ok || dup || (k != "id" && k != "event" && k != "data") {
				return progressed, nil, errFrame
			}
			frame[k] = v
			continue
		}
		switch frame["event"] {
		case "closed":
			r, err := client.DecodeResult([]byte(frame["data"]))
			if err != nil || len(frame) != 2 {
				return progressed, nil, errFrame
			}
			return progressed, &r, nil
		case "event":
			var e streamed
			seq, err := strconv.ParseInt(frame["id"], 10, 64)
			if err != nil || contract.UnmarshalStrict([]byte(frame["data"]), &e) != nil || e.Seq != seq || seq <= *last || e.Kind == "" {
				return progressed, nil, errFrame
			}
			if asJSON {
				err = sayJSON(out, []byte(frame["data"]))
			} else {
				err = describeEvent(out, e)
			}
			if err != nil {
				return progressed, nil, fmt.Errorf("writing the event: %w", err)
			}
			*last, progressed = seq, true
		default:
			return progressed, nil, errFrame
		}
		frame = map[string]string{}
	}
	if errors.Is(sc.Err(), bufio.ErrTooLong) {
		return progressed, nil, errFrame
	}
	return progressed, nil, nil // the stream ended: resumable
}

// describeEvent writes one event's line; the payload is never shown.
func describeEvent(w io.Writer, e streamed) error {
	project, withheld := "-", ""
	if e.Project != nil {
		project = *e.Project
	}
	if e.PayloadWithheld {
		withheld = " (payload withheld)"
	}
	return say(w, "%s %s %s by %s in %s%s\n", strconv.FormatInt(e.Seq, 10), e.Kind, e.Subject, e.Actor, project, withheld)
}
