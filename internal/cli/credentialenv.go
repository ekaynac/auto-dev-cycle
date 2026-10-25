// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
)

// devCredentialEnv runs `adc dev credential-env REGISTRY`: for each
// credential reference of the registry's executors, once, sorted, one line
// "REF NAME", NAME the environment variable adcd reads it from
// (gateway.EnvName, the one mapping; Astra #440 M7). Names only: it reads
// no environment and prints no value. A reference with no variable (one
// holding '_') is refused, named; an unreadable or invalid registry could
// not run.
func devCredentialEnv(args []string, stdout, stderr io.Writer) int {
	const name = "adc dev credential-env"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		return entry.ExitOK
	} else if err != nil || fs.NArg() != 1 {
		fmt.Fprintf(stderr, "%s: usage: %s REGISTRY\n", name, name)
		return entry.ExitUsage
	}
	doc, err := registry.Load(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "%s: could_not_run: the registry: %v\n", name, err)
		return entry.ExitCouldNotRun
	}
	var refs []string
	for _, e := range doc.Executors {
		if e.CredentialRef != nil && !slices.Contains(refs, *e.CredentialRef) {
			refs = append(refs, *e.CredentialRef)
		}
	}
	slices.Sort(refs)
	for _, ref := range refs {
		env, ok := gateway.EnvName(ref)
		if !ok {
			fmt.Fprintf(stderr, "%s: refused: the credential reference %s has no environment variable\n", name, ref)
			return entry.ExitRefused
		}
		fmt.Fprintf(stdout, "%s %s\n", ref, env)
	}
	return entry.ExitOK
}
