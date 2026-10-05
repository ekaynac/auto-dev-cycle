// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/fixtures"
)

// devFixtures runs `adc dev fixtures --contract [--only NAME] [--list]
// [--json]` (docs/design/m0/00 "M0 exit check"): every M0 contract fixture
// through the real components, each compared with its golden. Exit 0 when
// all pass, 1 when one fails, 2 when one could not run.
func devFixtures(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const name = "adc dev fixtures"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	contractSet := fs.Bool("contract", false, "run the M0 contract fixtures (required)")
	only := fs.String("only", "", "run only this fixture")
	list := fs.Bool("list", false, "list the fixtures and their mutations")
	asJSON := fs.Bool("json", false, "print one JSON document per fixture")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		return entry.ExitOK
	} else if err != nil {
		return entry.ExitUsage
	}
	if fs.NArg() > 0 || !*contractSet {
		fmt.Fprintf(stderr, "%s: usage: %s --contract [--only NAME] [--list] [--json]\n", name, name)
		return entry.ExitUsage
	}
	gs, err := fixtures.Goldens()
	if err != nil {
		fmt.Fprintf(stderr, "%s: could_not_run: %v\n", name, err)
		return entry.ExitCouldNotRun
	}
	if *only != "" {
		var pick []fixtures.Golden
		for _, g := range gs {
			if g.Name == *only {
				pick = append(pick, g)
			}
		}
		if len(pick) == 0 {
			fmt.Fprintf(stderr, "%s: no fixture %q\n", name, *only)
			return entry.ExitUsage
		}
		gs = pick
	}
	if *list {
		for _, g := range gs {
			fmt.Fprintf(stdout, "%s %s\n", g.Name, g.Mutation)
		}
		return entry.ExitOK
	}
	code := entry.ExitOK
	for _, g := range gs {
		r, err := fixtures.Run(ctx, g)
		if err != nil {
			fmt.Fprintf(stderr, "%s: could_not_run: %v\n", name, err)
			return entry.ExitCouldNotRun
		}
		if !r.Pass {
			code = entry.ExitRefused
		}
		if *asJSON {
			b, err := json.Marshal(struct {
				Name   string          `json:"name"`
				Pass   bool            `json:"pass"`
				Got    []fixtures.Step `json:"got"`
				Detail string          `json:"detail"`
			}{g.Name, r.Pass, r.Got, r.Detail})
			if err != nil {
				fmt.Fprintf(stderr, "%s: could_not_run: %v\n", name, err)
				return entry.ExitCouldNotRun
			}
			fmt.Fprintf(stdout, "%s\n", b)
			continue
		}
		status := "pass"
		if !r.Pass {
			status = "FAIL: " + r.Detail
		}
		fmt.Fprintf(stdout, "%s: %s\n", g.Name, status)
	}
	return code
}
