// SPDX-License-Identifier: Apache-2.0

package cli

// adc task show and adc task wait (M1 slice plan §2.3; P22a-7, W18), over
// the task read (GET /v1/tasks):
//   - adc task show TASK [--json] prints the task's record: its board card
//     and its first-attempt metrics. A task the caller cannot read, or no
//     task, is refused (exit 1).
//   - adc task wait TASK --until COLUMN[,COLUMN...] [--timeout D] [--every
//     D] [--json] reads the task every D until its card's column is one of
//     the columns, and prints its record then. The exit codes: 0, a column
//     reached; 1, the task is not readable; 5, the timeout passed first; 6,
//     the task ended (done or done_candidate) in a column not asked for; 2,
//     the controller could not be read; 64, a usage error.

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

// tasksResource is the task read's resource.
const tasksResource = "tasks"

// columns are the board's columns, and ended the ones a task never leaves.
var (
	columns = []types.BoardPlacedCardColumn{types.BoardPlacedCardColumnAwaitingApproval, types.BoardPlacedCardColumnReady, types.BoardPlacedCardColumnRunning,
		types.BoardPlacedCardColumnVerifying, types.BoardPlacedCardColumnDoneCandidate, types.BoardPlacedCardColumnDone}
	ended = []types.BoardPlacedCardColumn{types.BoardPlacedCardColumnDoneCandidate, types.BoardPlacedCardColumnDone}
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

// taskRecord reads task's record: its raw JSON and column ("" unplaced),
// or the exit code and message of a read that failed or found no task.
func taskRecord(ctx context.Context, name string, task types.TaskID, stderr io.Writer, c Client) (json.RawMessage, types.BoardPlacedCardColumn, int, bool) {
	r, ok := c.(Reader)
	if !ok {
		return nil, "", failed(stderr, name, ErrNoTransport), false
	}
	raw, err := r.Read(ctx, tasksResource)
	if err != nil {
		return nil, "", failed(stderr, name, err), false
	}
	var list struct{ Items []json.RawMessage }
	if err := json.Unmarshal(raw, &list); err != nil {
		say(stderr, "%s: could_not_run: the controller's read is not a list\n", name)
		return nil, "", entry.ExitCouldNotRun, false
	}
	for _, item := range list.Items {
		var rec types.TaskRead
		if json.Unmarshal(item, &rec) != nil || rec.ID != task {
			continue
		}
		var column types.BoardPlacedCardColumn
		if placed, ok := rec.Card.Variant.(*types.BoardPlacedCard); ok {
			column = placed.Column
		}
		return item, column, entry.ExitOK, true
	}
	say(stderr, "%s: refused: no task %s that you read\n", name, task)
	return nil, "", entry.ExitRefused, false
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
	rec, _, code, ok := taskRecord(ctx, name, task, stderr, c)
	if !ok {
		return code
	}
	return printTask(name, rec, *asJSON, stdout, stderr)
}

func taskWait(ctx context.Context, args []string, stdout, stderr io.Writer, c Client) int {
	const name = "adc task wait"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	until := fs.String("until", "", "the columns to wait for, comma-separated: awaiting_approval, ready, running, verifying, done_candidate, done (required)")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait at most")
	every := fs.Duration("every", time.Second, "how often to read the task (at least 100ms)")
	asJSON := fs.Bool("json", false, "print the task's record as one JSON document")
	task, code, ok := taskArgs(fs, args, stderr)
	if !ok {
		return code
	}
	var want []types.BoardPlacedCardColumn
	for _, s := range strings.Split(*until, ",") {
		col := types.BoardPlacedCardColumn(strings.TrimSpace(s))
		if !slices.Contains(columns, col) {
			say(stderr, "%s: --until takes board columns; %q is none\n", name, col)
			return entry.ExitUsage
		}
		want = append(want, col)
	}
	if *timeout <= 0 || *every < 100*time.Millisecond {
		say(stderr, "%s: a positive --timeout and --every of at least 100ms\n", name)
		return entry.ExitUsage
	}
	deadline := time.Now().Add(*timeout)
	for {
		rec, column, code, ok := taskRecord(ctx, name, task, stderr, c)
		switch {
		case !ok:
			return code
		case slices.Contains(want, column):
			return printTask(name, rec, *asJSON, stdout, stderr)
		case slices.Contains(ended, column):
			say(stderr, "%s: task %s ended in %s, not %s\n", name, task, column, *until)
			return entry.ExitEnded
		case !time.Now().Before(deadline):
			say(stderr, "%s: timed out after %s: task %s is in %q\n", name, *timeout, task, column)
			return entry.ExitTimedOut
		}
		select {
		case <-ctx.Done():
			say(stderr, "%s: could_not_run: %s\n", name, ctx.Err())
			return entry.ExitCouldNotRun
		case <-time.After(min(*every, time.Until(deadline))):
		}
	}
}
