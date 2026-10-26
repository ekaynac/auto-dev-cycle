// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/referee"
)

// devReferee is `adc dev referee --item FILE --patch FILE --out FILE
// [--podman PATH]` (stage 2, P-A; internal/referee): it judges the patch on
// the item and writes the verdict record to --out, a new file (never an
// existing one). It prints the verdict, and exits 0 accepted, 1 rejected or
// invalid_item, 2 could_not_run.
func devReferee(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const name = "adc dev referee"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	item := fs.String("item", "", "the item, a schemas/testing/referee-item.json document (required)")
	patch := fs.String("patch", "", "the agent's patch, a git diff against the item's base; may be empty (required)")
	out := fs.String("out", "", "the verdict record's file, created; never an existing file (required)")
	podman := fs.String("podman", "", "the podman binary (default: podman on PATH)")
	if code, ok := parse(fs, args, stderr); !ok {
		return code
	}
	if fs.NArg() > 0 || *item == "" || *patch == "" || *out == "" {
		say(stderr, "%s: --item FILE, --patch FILE and --out FILE are required, and nothing else\n", name)
		return entry.ExitUsage
	}
	itemDir, err := filepath.Abs(filepath.Dir(*item))
	var itemBytes, patchBytes []byte
	if err == nil {
		itemBytes, err = referee.ReadFile(*item, referee.MaxItem)
	}
	if err == nil {
		patchBytes, err = referee.ReadFile(*patch, referee.MaxPatch)
	}
	if err != nil {
		say(stderr, "%s: the item or the patch cannot be read\n", name)
		return entry.ExitUsage
	}
	if *podman == "" {
		if *podman, err = exec.LookPath("podman"); err != nil {
			say(stderr, "%s: could_not_run: no podman on PATH\n", name)
			return entry.ExitCouldNotRun
		}
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		say(stderr, "%s: the output file exists or cannot be created\n", name)
		return entry.ExitUsage
	}
	rep := referee.Judge(ctx, referee.In{Podman: *podman, Item: itemBytes, ItemDir: itemDir, Patch: patchBytes})
	b, err := json.MarshalIndent(rep, "", "  ")
	if err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err = errors.Join(err, f.Close()); err != nil {
		say(stderr, "%s: could_not_run: writing the verdict record\n", name)
		return entry.ExitCouldNotRun
	}
	say(stdout, "%s\n", rep.Verdict)
	switch rep.Verdict {
	case referee.Accepted:
		return entry.ExitOK
	case referee.CouldNotRun:
		return entry.ExitCouldNotRun
	}
	return entry.ExitRefused
}
