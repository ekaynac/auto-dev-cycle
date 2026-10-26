// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/regularfile"
	"github.com/ekaynac/auto-dev-cycle/internal/secretscan"
)

// devScan runs `adc dev scan`, the installed cycle harness's one search
// (#226 entry 206; secretscan):
//
//	adc dev scan [--secrets FILE] [--canary FILE] [--name NAME]... [--own PATH]... [--links-inside] [--depth N] [--null] ROOT...
//	adc dev scan --print-forms --secrets FILE
//
// --secrets: one secret a line (secretscan.SecretForms); --canary: the
// canary, its first line (secretscan.CanaryForms, JSON decoded too). Each
// FILE is "-" (stdin, once) or a regular file of this user's, mode 0600: a
// secret is never an argument. --name: an entry of that name below a root
// is a finding; --own: a regular file whose content is its own (a link to
// it is a finding); --links-inside: a link out of the roots is an
// incomplete search (the run's own directory; never a sandbox root's);
// --depth: at most N below a root. No link is followed. It prints each
// finding's path (quoted, or NUL-terminated with --null), every form of
// every secret and canary in it redacted, and exits 0 when there is none,
// 1 when there is one, 2 when the search could not complete or its input
// could not be read: one fixed line on stderr, naming no path and no
// secret. --print-forms prints the secrets' forms, one a line, for the
// driver's redaction (secrets.sh's known).
func devScan(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	const name = "adc dev scan"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	secrets := fs.String("secrets", "", "")
	canary := fs.String("canary", "", "")
	depth := fs.Int("depth", 0, "")
	null := fs.Bool("null", false, "")
	printForms := fs.Bool("print-forms", false, "")
	linksInside := fs.Bool("links-inside", false, "")
	var names, own multi
	fs.Var(&names, "name", "")
	fs.Var(&own, "own", "")
	usage := func() int {
		fmt.Fprintf(stderr, "%s: usage: %s [--secrets FILE] [--canary FILE] [--name NAME]... [--own PATH]... [--depth N] [--null] ROOT...\n", name, name)
		return entry.ExitUsage
	}
	if err := fs.Parse(args); err != nil {
		return usage()
	}
	if *secrets == "-" && *canary == "-" {
		return usage()
	}
	if *printForms != (fs.NArg() == 0) || (*printForms && (*secrets == "" || *canary != "" || len(names)+len(own) > 0)) ||
		(!*printForms && *secrets == "" && *canary == "" && len(names) == 0) {
		return usage()
	}
	for _, n := range names {
		if n == "" || strings.ContainsRune(n, '/') {
			return usage()
		}
	}
	cannot := func() int {
		fmt.Fprintf(stderr, "%s: could_not_run: the search did not complete\n", name)
		return entry.ExitCouldNotRun
	}
	var forms []string
	if *secrets != "" {
		lines, err := readInput(*secrets, stdin)
		if err != nil || len(lines) == 0 {
			return cannot()
		}
		for _, s := range lines {
			f, err := secretscan.SecretForms(s)
			if err != nil {
				return cannot()
			}
			forms = append(forms, f...)
		}
	}
	if *printForms {
		for _, f := range forms {
			fmt.Fprintln(stdout, f)
		}
		return entry.ExitOK
	}
	search := secretscan.Search{Names: names, Own: own, Depth: *depth, LinksInside: *linksInside}
	if *canary != "" {
		lines, err := readInput(*canary, stdin)
		if err != nil || len(lines) != 1 {
			return cannot()
		}
		f, err := secretscan.CanaryForms(lines[0])
		if err != nil {
			return cannot()
		}
		forms = append(forms, f...)
		search.Decode = true
	}
	search.Forms = forms
	// The search ends with adc's context (its first signal): never a scan that cannot be stopped (Opus #443 r3 O1).
	found, err := search.Run(ctx, fs.Args())
	if err != nil {
		return cannot()
	}
	for _, p := range found {
		for _, f := range forms {
			p = strings.ReplaceAll(p, f, "[redacted]")
		}
		if *null {
			fmt.Fprintf(stdout, "%s\x00", p)
		} else {
			fmt.Fprintln(stdout, strconv.Quote(p))
		}
	}
	if len(found) > 0 {
		return entry.ExitRefused
	}
	return entry.ExitOK
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// readInput: the lines of FILE ("-": stdin), empty ones dropped. A file must
// be a regular file of this user's, mode 0600 (no group or other bits).
func readInput(file string, stdin io.Reader) ([]string, error) {
	var b []byte
	if file == "-" {
		var err error
		if b, err = io.ReadAll(stdin); err != nil {
			return nil, err
		}
	} else {
		f, err := os.OpenFile(file, regularfile.Flags, 0)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		fi, err := regularfile.Check(f, nil)
		if err != nil {
			return nil, err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if fi.Mode().Perm()&0o077 != 0 || !ok || int(st.Uid) != os.Getuid() {
			return nil, errors.New("not a private file of this user's")
		}
		if b, err = io.ReadAll(f); err != nil {
			return nil, err
		}
	}
	// Split on "\n" alone: a CR is the secret's own byte, never a line end
	// dropped (its base64 and hex forms are of the whole; Opus #443 r3 O2).
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}
