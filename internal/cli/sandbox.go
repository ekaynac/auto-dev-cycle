// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"flag"
	"io"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

// Prober is a Client that also runs the sandbox probe.
type Prober interface {
	SandboxProbe(ctx context.Context, image, mode string) ([]byte, error)
}

// sandboxProbe is `adc dev sandbox probe --image REF@DIGEST --mode
// gate|build` (P9): the controller runs the fixed probe in a fresh sandbox
// run; any violation exits refused.
func sandboxProbe(ctx context.Context, args []string, stdout, stderr io.Writer, c Client) int {
	const name = "adc dev sandbox probe"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	image := fs.String("image", "", "a fully qualified image reference pinned by digest (required)")
	mode := fs.String("mode", "", "gate or build (required)")
	asJSON := fs.Bool("json", false, "print the controller's report as one JSON document")
	if code, ok := parse(fs, args, stderr); !ok {
		return code
	}
	if fs.NArg() > 0 || *image == "" || *mode != "gate" && *mode != "build" {
		say(stderr, "%s: --image REF@DIGEST and --mode gate|build are required, and nothing else\n", name)
		return entry.ExitUsage
	}
	p, ok := c.(Prober)
	if !ok {
		return failed(stderr, name, ErrNoTransport)
	}
	raw, err := p.SandboxProbe(ctx, *image, *mode)
	if err != nil {
		return failed(stderr, name, err)
	}
	var rep types.SandboxProbe
	if err = json.Unmarshal(raw, &rep); err == nil && *asJSON {
		err = sayJSON(stdout, raw)
	} else if err == nil { // --json carries the violations themselves
		err = say(stdout, "%s (%s): %s violation(s)\n", rep.Image, rep.Mode, len(rep.Violations)) // say prints every value as a string
	}
	if err != nil {
		say(stderr, "%s: could_not_run: writing the result: %s\n", name, err)
		return entry.ExitCouldNotRun
	}
	if len(rep.Violations) > 0 {
		return entry.ExitRefused
	}
	return entry.ExitOK
}
