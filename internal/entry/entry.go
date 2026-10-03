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
)

// Exit codes from the single exit-code table (docs/design/m0/08 "CLI rules").
const (
	ExitOK          = 0
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
