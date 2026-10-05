// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// trustedHead is SEQ:HASH, the head a backup carries (03 rule 3).
var trustedHead = regexp.MustCompile(`^(0|[1-9][0-9]{0,17}):(sha256:[0-9a-f]{64})$`)

// verifyLog runs `adc dev verify-log --state DIR [--trusted SEQ:HASH]
// [--json]` (docs/design/m0/00 "M0 exit check"): it takes the state
// directory's lock as a probe, which never writes the lock file, and
// verifies the existing store's whole hash chain read-only, against a
// trusted head when one is given (03 rule 3). It never creates, migrates
// or repairs a store.
func verifyLog(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const name = "adc dev verify-log"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	state := fs.String("state", "", "the controller's state directory (required)")
	trusted := fs.String("trusted", "", "a trusted head, SEQ:sha256:HEX, from a backup")
	asJSON := fs.Bool("json", false, "print the result as one JSON document")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		return entry.ExitOK
	} else if err != nil {
		return entry.ExitUsage
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, name+": "+format+"\n", a...)
		return entry.ExitUsage
	}
	if fs.NArg() > 0 || *state == "" {
		return usage("usage: %s --state DIR [--trusted SEQ:sha256:HEX] [--json]", name)
	}
	// An explicitly empty --trusted is malformed, never "no anchor".
	var head *eventstore.Head
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "trusted" {
			head = &eventstore.Head{}
		}
	})
	if head != nil {
		m := trustedHead.FindStringSubmatch(*trusted)
		if m == nil {
			return usage("--trusted %q is not SEQ:sha256:HEX", *trusted)
		}
		head.Seq, _ = strconv.ParseInt(m[1], 10, 64) // at most 18 digits: fits
		head.Hash = m[2]
	}
	report := func(r vocab.Result, h *eventstore.Head, code int) int {
		if *asJSON {
			// The result goes through the vocabulary's writer check; the
			// head sits beside it.
			rb, err := json.Marshal(r)
			var doc map[string]any
			if err == nil {
				err = json.Unmarshal(rb, &doc)
			}
			var b []byte
			if err == nil {
				doc["head"] = h
				b, err = json.Marshal(doc)
			}
			if err != nil {
				fmt.Fprintf(stderr, "%s: could_not_run: %v\n", name, err)
				return entry.ExitCouldNotRun
			}
			fmt.Fprintf(stdout, "%s\n", b)
		} else if h != nil {
			fmt.Fprintf(stdout, "%s/%s: head %d %s\n", r.Outcome, r.Reason, h.Seq, h.Hash)
		} else {
			fmt.Fprintf(stdout, "%s/%s: %s\n", r.Outcome, r.Reason, r.Detail)
		}
		return code
	}
	// The check could not be performed: a result on stdout, the diagnostic
	// on stderr, never success.
	unreadable := func(err error) int {
		fmt.Fprintf(stderr, "%s: could_not_run: %v\n", name, err)
		return report(vocab.Result{Outcome: vocab.CouldNotRun, Reason: vocab.ReasonStoreUnreadable, Detail: err.Error()}, nil, entry.ExitCouldNotRun)
	}
	if _, err := os.Lstat(filepath.Join(*state, eventstore.DBFile)); errors.Is(err, os.ErrNotExist) {
		return usage("no event store in %s", *state)
	} else if err != nil {
		return unreadable(err)
	}
	lock, err := eventstore.ProbeLock(*state)
	if errors.Is(err, eventstore.ErrAlreadyRunning) {
		return report(vocab.Result{Outcome: vocab.CouldNotRun, Reason: vocab.ReasonControllerAlreadyRunning, Detail: err.Error()}, nil, entry.ExitCouldNotRun)
	}
	if err != nil {
		return unreadable(err)
	}
	defer lock.Release()
	h, err := eventstore.VerifyExisting(ctx, lock, head)
	if errors.Is(err, eventstore.ErrChainBroken) {
		return report(vocab.Result{Outcome: vocab.Failed, Reason: vocab.ReasonLogIntegrityBroken, Detail: err.Error()}, nil, entry.ExitRefused)
	}
	if err != nil {
		return unreadable(err)
	}
	return report(vocab.Result{Outcome: vocab.Empty, Reason: vocab.ReasonCompletedClean, Detail: ""}, &h, entry.ExitOK)
}
