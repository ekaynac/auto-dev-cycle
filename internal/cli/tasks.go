// SPDX-License-Identifier: Apache-2.0

package cli

// adc task show and adc task wait (M1 slice plan §2.1 step 5, §2.3; P22a-7,
// W18), over the one task's read (GET /v1/tasks/{task}; Opus #430 H1):
//   - adc task show TASK [--json] prints the task's record: its board card
//     and its first-attempt metrics. A task the caller cannot read, or no
//     task, is refused (exit 1).
//   - adc task wait TASK --until closed|held [--timeout D] [--every D]
//     [--json] reads the task every D until it is in one of the states
//     asked for, and prints its record then. closed: the task is terminal
//     (its card's column done_candidate or done); held: a node of its card
//     carries a hold badge. A state counts only when a read sees it: one
//     entered and left between two reads is not seen. Every read is bounded
//     by the timeout's deadline. A read the controller could not answer is
//     retried at the next tick until the deadline; a refusal ends the wait.
//   - The wait's exit codes: 0, a state asked for was seen; 1, the task is
//     not readable; 5, the timeout passed first; 6, the task closed when
//     only held was asked for, or its card is unplaced (its records
//     determine no column); 2, the context ended; 64, a usage error.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// tasksResource is the task read's resource; OneTask names the one task's
// read (GET /v1/tasks/{task}) among the reads (ReadCommands, the parity
// harness).
const (
	tasksResource = "tasks"
	OneTask       = "task"
)

// The states adc task wait waits for.
const (
	stateClosed = "closed"
	stateHeld   = "held"
)

// taskArgs parses args into fs: the task id, the first argument, then the
// flags; with no task first, only --help is answered (exit 0).
func taskArgs(fs *flag.FlagSet, args []string, stderr io.Writer) (types.TaskID, int, bool) {
	task, rest := types.TaskID(""), args
	if len(args) > 0 && strings.HasPrefix(args[0], "tsk_") {
		task, rest = types.TaskID(args[0]), args[1:]
	}
	if code, ok := parse(fs, rest, stderr); !ok {
		return "", code, false
	}
	if task == "" || fs.NArg() > 0 {
		say(stderr, "%s: the first argument is the task (tsk_...), then the flags\n", fs.Name())
		return "", entry.ExitUsage, false
	}
	return task, entry.ExitOK, true
}

// taskState is what a wait reads of a task's card.
type taskState struct {
	closed, held, unplaced bool
	column                 types.BoardPlacedCardColumn
}

// TaskReader is a Client that reads one task (client.Transport.ReadTask:
// GET /v1/tasks/{task}, the tasks list holding that task alone).
type TaskReader interface {
	ReadTask(ctx context.Context, task string) ([]byte, error)
}

// readTask reads task's record over the one task's read, never the whole
// list (Opus #430 H1): its raw JSON and state; refused when the read found
// no task the caller reads.
func readTask(ctx context.Context, task types.TaskID, c Client) (raw json.RawMessage, st taskState, refused bool, err error) {
	r, ok := c.(TaskReader)
	if !ok {
		return nil, st, false, ErrNoTransport
	}
	doc, err := r.ReadTask(ctx, string(task))
	if err != nil {
		return nil, st, false, err
	}
	var list struct{ Items []json.RawMessage }
	if err := json.Unmarshal(doc, &list); err != nil {
		return nil, st, false, errors.New("the controller's read is not a list")
	}
	for _, item := range list.Items {
		var rec types.TaskRead
		if json.Unmarshal(item, &rec) != nil || rec.ID != task {
			continue
		}
		placed, ok := rec.Card.Variant.(*types.BoardPlacedCard)
		if !ok {
			return item, taskState{unplaced: true}, false, nil
		}
		st.column = placed.Column
		st.closed = placed.Column == types.BoardPlacedCardColumnDoneCandidate || placed.Column == types.BoardPlacedCardColumnDone
		for _, n := range placed.Nodes {
			st.held = st.held || len(n.Holds) > 0
		}
		return item, st, false, nil
	}
	return nil, st, true, nil
}

// taskRecord is readTask for show: the record, or the exit code of a read
// that failed or found no task, its message said.
func taskRecord(ctx context.Context, name string, task types.TaskID, stderr io.Writer, c Client) (json.RawMessage, int, bool) {
	raw, _, refused, err := readTask(ctx, task, c)
	switch {
	case err != nil:
		return nil, failed(stderr, name, err), false
	case refused:
		say(stderr, "%s: refused: no task %s that you read\n", name, task)
		return nil, entry.ExitRefused, false
	}
	return raw, entry.ExitOK, true
}

// printTask prints rec: as is with asJSON, else as a rendered record.
func printTask(name string, rec json.RawMessage, asJSON bool, stdout, stderr io.Writer) int {
	var err error
	if asJSON {
		err = sayJSON(stdout, rec)
	} else {
		err = render(stdout, tasksResource, append(append([]byte(`{"items":[`), rec...), "]}"...))
	}
	if err != nil {
		say(stderr, "%s: could_not_run: writing the result: %s\n", name, err)
		return entry.ExitCouldNotRun
	}
	return entry.ExitOK
}

func taskShow(ctx context.Context, args []string, stdout, stderr io.Writer, c Client) int {
	const name = "adc task show"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the task's record as one JSON document")
	task, code, ok := taskArgs(fs, args, stderr)
	if !ok {
		return code
	}
	rec, code, ok := taskRecord(ctx, name, task, stderr, c)
	if !ok {
		return code
	}
	return printTask(name, rec, *asJSON, stdout, stderr)
}

func taskWait(ctx context.Context, args []string, stdout, stderr io.Writer, c Client) int {
	const name = "adc task wait"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	until := fs.String("until", "", "the states to wait for, comma-separated: closed (the task is terminal), held (a node of it is held) (required)")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait at most")
	every := fs.Duration("every", time.Second, "how often to read the task (at least 100ms)")
	asJSON := fs.Bool("json", false, "print the task's record as one JSON document")
	task, code, ok := taskArgs(fs, args, stderr)
	if !ok {
		return code
	}
	var closed, held bool
	for _, s := range strings.Split(*until, ",") {
		switch strings.TrimSpace(s) {
		case stateClosed:
			closed = true
		case stateHeld:
			held = true
		default:
			say(stderr, "%s: --until takes closed or held; %q is neither\n", name, s)
			return entry.ExitUsage
		}
	}
	if *timeout <= 0 || *every < 100*time.Millisecond {
		say(stderr, "%s: a positive --timeout and --every of at least 100ms\n", name)
		return entry.ExitUsage
	}
	wctx, cancel := context.WithTimeout(ctx, *timeout) // every read is bounded by the deadline too
	defer cancel()
	var last error // the last read the controller could not answer
	for {
		rec, st, refused, err := readTask(wctx, task, c)
		switch {
		case ctx.Err() != nil:
			say(stderr, "%s: could_not_run: %s\n", name, ctx.Err())
			return entry.ExitCouldNotRun
		case wctx.Err() != nil:
			say(stderr, "%s: timed out after %s waiting for task %s to be %s (last read: %v)\n", name, *timeout, task, *until, errors.Join(err, last))
			return entry.ExitTimedOut
		case permanent(err): // a lost token or grant never recovers by waiting (Astra #335 M2)
			return failed(stderr, name, err)
		case err != nil:
			last = err // a transport failure or could_not_run: retried at the next tick
		case refused:
			say(stderr, "%s: refused: no task %s that you read\n", name, task)
			return entry.ExitRefused
		case closed && st.closed, held && st.held:
			return printTask(name, rec, *asJSON, stdout, stderr)
		case st.unplaced:
			say(stderr, "%s: task %s's card is unplaced: its records determine no column\n", name, task)
			return entry.ExitEnded
		case st.closed:
			say(stderr, "%s: task %s closed (%s) without being %s\n", name, task, st.column, *until)
			return entry.ExitEnded
		}
		select {
		case <-wctx.Done():
		case <-time.After(*every):
		}
	}
}

// permanent: err is a refusal no later read undoes: the bearer no longer
// authenticates (401), a read refused failed (403 failed/*: the grant is
// gone), a request the controller cannot parse, or a controller that
// serves no task read (404: an adcd from before it, Mythos #353 L1), which
// ends as adc task show does. A transport failure and a could_not_run
// answer are not.
func permanent(err error) bool {
	var refused *client.Refused
	switch {
	case errors.As(err, &refused):
		return refused.Result.Outcome == vocab.Failed
	case errors.Is(err, client.ErrUnauthenticated), errors.Is(err, client.ErrBadRequest), errors.Is(err, client.ErrNoResource):
		return true
	}
	return false
}
