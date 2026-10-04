// SPDX-License-Identifier: Apache-2.0

// Package entry is the shared command-line entry of the M0 binary skeletons.
// Until each binary gains its real command tree, it answers `version` and
// refuses everything else with the exit codes of docs/design/m0/08.
package entry

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/ekaynac/auto-dev-cycle/internal/buildinfo"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Exit codes from the single exit-code table (docs/design/m0/08 "CLI rules").
const (
	ExitOK          = 0
	ExitRefused     = 1
	ExitCouldNotRun = 2
	ExitUsage       = 64
)

// Main runs the skeleton command line for binary and returns the exit code.
func Main(binary string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "%s: could_not_run: no command implemented yet in this build; try `%s version`\n", binary, binary)
		return ExitCouldNotRun
	}
	switch args[0] {
	case "version":
		return version(binary, args[1:], stdout, stderr)
	case "dev":
		if binary == "adc" && len(args) >= 3 && args[1] == "vocab" && args[2] == "check" {
			return vocabCheck(args[3:], stdout, stderr)
		}
		fmt.Fprintf(stderr, "%s: unknown dev command; usage: adc dev vocab check [--root DIR] [--json]\n", binary)
		return ExitUsage
	case "help", "-h", "--help":
		fmt.Fprintf(stdout, "usage: %s version [--json]\n", binary)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "%s: unknown command %q\nusage: %s version [--json]\n", binary, args[0], binary)
		return ExitUsage
	}
}

func version(binary string, args []string, stdout, stderr io.Writer) int {
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			fmt.Fprintf(stderr, "%s version: unknown flag %q\n", binary, a)
			return ExitUsage
		}
		asJSON = true
	}
	info := buildinfo.Get(binary)
	if !asJSON {
		fmt.Fprintln(stdout, info.String())
		return ExitOK
	}
	if err := json.NewEncoder(stdout).Encode(info); err != nil {
		fmt.Fprintf(stderr, "%s version: could_not_run: writing JSON: %v\n", binary, err)
		return ExitCouldNotRun
	}
	return ExitOK
}

// vocabCheck runs `adc dev vocab check`: the registries load and cross-check,
// and the Go sources under --root name reasons only through the constants.
func vocabCheck(args []string, stdout, stderr io.Writer) int {
	root, asJSON := ".", false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--json":
			asJSON = true
		case args[i] == "--root" && i+1 < len(args):
			root = args[i+1]
			i++
		default:
			fmt.Fprintf(stderr, "adc dev vocab check: unknown flag %q\n", args[i])
			return ExitUsage
		}
	}
	// could not run: the diagnostic on stderr and, in JSON mode, one result
	// document on stdout (note 08).
	couldNotRun := func(err error) int {
		fmt.Fprintf(stderr, "adc dev vocab check: could_not_run: %v\n", err)
		if asJSON {
			// A bootstrap document: the registry may be the thing that failed,
			// so it is not marshalled through the vocabulary. Its outcome and
			// reason are fixed, registered values.
			doc := struct {
				Outcome string `json:"outcome"`
				Reason  string `json:"reason"`
				Detail  string `json:"detail"`
			}{string(vocab.CouldNotRun), vocab.ReasonCheckUnloadable.String(), err.Error()}
			if werr := json.NewEncoder(stdout).Encode(doc); werr != nil {
				fmt.Fprintf(stderr, "adc dev vocab check: could_not_run: writing JSON: %v\n", werr)
			}
		}
		return ExitCouldNotRun
	}
	reg, err := vocab.Load()
	if err != nil {
		return couldNotRun(fmt.Errorf("registry: %w", err))
	}
	findings, err := reg.CheckSource(root)
	if err != nil {
		return couldNotRun(err)
	}
	if asJSON {
		if findings == nil {
			findings = []vocab.Finding{}
		}
		if err := json.NewEncoder(stdout).Encode(map[string]any{"reasons": len(reg.Reasons), "holds": len(reg.Holds), "findings": findings}); err != nil {
			fmt.Fprintf(stderr, "adc dev vocab check: could_not_run: %v\n", err)
			return ExitCouldNotRun
		}
	} else {
		var werr error
		for _, f := range findings {
			if _, err := fmt.Fprintf(stdout, "%s: %s\n", f.Pos, f.Problem); err != nil && werr == nil {
				werr = err
			}
		}
		if _, err := fmt.Fprintf(stdout, "vocab check: %d reasons, %d holds registered; %d finding(s)\n", len(reg.Reasons), len(reg.Holds), len(findings)); err != nil && werr == nil {
			werr = err
		}
		if werr != nil {
			fmt.Fprintf(stderr, "adc dev vocab check: could_not_run: writing the report: %v\n", werr)
			return ExitCouldNotRun
		}
	}
	if len(findings) > 0 {
		return ExitRefused
	}
	return ExitOK
}
