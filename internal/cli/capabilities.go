// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/capability"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

// capabilityRow is one required inventory row of a capability, as
// `adc dev capabilities --json` prints it.
type capabilityRow struct {
	Invariant string `json:"invariant"`
	Check     string `json:"check"`
	Kept      bool   `json:"kept"`
}

// capabilityView is one capability, as `adc dev capabilities --json`
// prints it.
type capabilityView struct {
	Name        string          `json:"name"`
	Checkpoint  string          `json:"checkpoint"`
	ActivatedBy string          `json:"activated_by"`
	Active      bool            `json:"active"`
	Actions     []string        `json:"actions"`
	EffectKinds []string        `json:"effect_kinds"`
	Rows        []capabilityRow `json:"rows"`
	Pending     []string        `json:"pending"`
}

// devCapabilities runs `adc dev capabilities [--json]`: this build's
// activation table (capability.Table; M1 slice plan §1), which adcd of the
// same build gates its actions and effect kinds by. Each capability is
// active or not, with the rows it still needs and what it waits for.
func devCapabilities(args []string, stdout, stderr io.Writer) int {
	const name = "adc dev capabilities"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the table as one JSON document")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		return entry.ExitOK
	} else if err != nil || fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: usage: %s [--json]\n", name, name)
		return entry.ExitUsage
	}
	views := make([]capabilityView, 0, len(capability.Table))
	for _, c := range capability.Table {
		v := capabilityView{Name: c.Name, Checkpoint: c.Checkpoint, ActivatedBy: c.ActivatedBy, Active: c.Active(),
			Actions: append([]string{}, c.Actions...), EffectKinds: append([]string{}, c.EffectKinds...), Rows: []capabilityRow{}, Pending: append([]string{}, c.Pending...)}
		for _, r := range c.Rows {
			v.Rows = append(v.Rows, capabilityRow{r.Invariant, r.Check, capability.Kept(r)})
		}
		views = append(views, v)
	}
	if *asJSON {
		b, err := json.Marshal(map[string]any{"capabilities": views})
		if err != nil {
			fmt.Fprintf(stderr, "%s: could_not_run: %v\n", name, err)
			return entry.ExitCouldNotRun
		}
		fmt.Fprintf(stdout, "%s\n", b)
		return entry.ExitOK
	}
	for _, v := range views {
		if v.Active {
			fmt.Fprintf(stdout, "%s: active\n", v.Name)
			continue
		}
		var missing []string
		for _, r := range v.Rows {
			if !r.Kept {
				missing = append(missing, r.Invariant+" "+r.Check)
			}
		}
		fmt.Fprintf(stdout, "%s: inactive until %s; rows not kept: %s; pending: %s\n", v.Name, v.ActivatedBy, orNone(missing), orNone(v.Pending))
	}
	return entry.ExitOK
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}
