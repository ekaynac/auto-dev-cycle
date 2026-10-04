// SPDX-License-Identifier: Apache-2.0

// Package profile loads and checks project profiles (docs/design/m0/07):
// the schema first, then the semantic checks the schema cannot express.
// Validation runs before adoption, never after.
//
// The coverage of protected documents and topology files by protected_paths
// is a separate change. Two checks of note 07 need what M0 does not have yet
// and land with it:
// that the base ref exists (repository access) and that the data policy is
// within the installation's usage profile (the usage profiles, M2).
package profile

import (
	"errors"
	"fmt"
	"regexp/syntax"
	"strconv"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

// Schema is the profile's contract schema.
const Schema = "config/project-profile"

// Load reads a profile (YAML or JSON, at most MaxBytes), validates it
// against the schema and runs the semantic checks. Any problem refuses the
// whole profile.
func Load(raw []byte) (*types.ProjectProfile, error) {
	js, err := toJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	p, err := contract.Decode[types.ProjectProfile](Schema, js)
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	if err := Check(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Check runs the semantic checks on a schema-valid profile and reports every
// problem found, each prefixed by its field.
func Check(p *types.ProjectProfile) error {
	var errs []error
	bad := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("profile: %s: %s", field, fmt.Sprintf(format, args...)))
	}

	if err := checkRefName(p.Repo.BaseRef); err != nil {
		bad("repo.base_ref", "%q: %v", p.Repo.BaseRef, err)
	}
	// A prefix is valid when a branch name completes it to a valid branch.
	if err := checkBranchName(p.Vocabulary.BranchPrefix + "x"); err != nil {
		bad("vocabulary.branch_prefix", "%q: %v", p.Vocabulary.BranchPrefix, err)
	}

	commands := map[string][]types.Argv{
		"package.recipe":                      p.Package.Recipe,
		"toolchain.dependency_recipe.command": {p.Toolchain.DependencyRecipe.Command},
		"gate.command":                        {p.Gate.Command},
	}
	if c := p.RuntimeVerification.Command; c != nil {
		commands["runtime_verification.command"] = []types.Argv{*c}
	}
	for field, argvs := range commands {
		for _, argv := range argvs {
			if err := checkArgv(argv); err != nil {
				bad(field, "%v", err)
			}
		}
	}

	repoPaths := func(field string, ps []types.RepoPath) {
		for _, x := range ps {
			if err := checkRepoPath(string(x)); err != nil {
				bad(field, "%q: %v", x, err)
			}
		}
	}
	repoPaths("package.outputs", p.Package.Outputs)
	repoPaths("toolchain.dependency_recipe.lockfiles", p.Toolchain.DependencyRecipe.Lockfiles)
	if t := p.Integration.Topology; t != nil {
		repoPaths("integration.topology.files", t.Files)
	}

	// Protected globs and their coverage of protected documents and topology
	// files are checked by the coverage checks (a separate change).

	if p.Forge.Kind == types.ProjectProfileForgeKindGit {
		c := p.Forge.Capabilities
		if c.ChangeRequests || c.ProtectedBranches || c.RequiredChecks {
			bad("forge.capabilities", "plain git has no change requests, protected branches or required checks")
		}
	}

	envs := map[types.EnvironmentName]bool{}
	for _, e := range p.Gate.Environments {
		if envs[e.Name] {
			bad("gate.environments", "%q is named twice", e.Name)
		}
		envs[e.Name] = true
	}
	switch rv := p.RuntimeVerification; {
	case (rv.Command == nil) != (rv.FactsFrom == nil):
		bad("runtime_verification", "facts_from is null exactly when command is null")
	case rv.FactsFrom != nil && !envs[*rv.FactsFrom]:
		bad("runtime_verification.facts_from", "%q is not a gate environment", *rv.FactsFrom)
	}

	task, terr := cents(string(p.Budget.Rails.PerTaskUsd))
	day, derr := cents(string(p.Budget.Rails.PerDayUsd))
	if err := errors.Join(terr, derr); err != nil {
		bad("budget.rails", "%v", err)
	} else if task > day {
		bad("budget.rails", "per_task_usd %s exceeds per_day_usd %s", p.Budget.Rails.PerTaskUsd, p.Budget.Rails.PerDayUsd)
	}

	if re := p.Vocabulary.TaskIDPattern; re != nil {
		if err := checkPattern(*re); err != nil {
			bad("vocabulary.task_id_pattern", "%v", err)
		}
	}
	return errors.Join(errs...)
}

// checkArgv: a command runs without a shell, so its executable is named and
// no argument carries a NUL (which no exec call can pass). Other arguments
// may be empty.
func checkArgv(argv types.Argv) error {
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("a command names its executable")
	}
	for i, a := range argv {
		if strings.ContainsRune(a, 0) {
			return fmt.Errorf("argument %d contains a NUL byte", i)
		}
	}
	return nil
}

// maxPatternBytes bounds a task id pattern. Only its syntax is checked here:
// parsing never expands counted repetitions, so a short pattern cannot make
// validation allocate (compiling one could).
const maxPatternBytes = 256

// checkPattern checks that re is a valid RE2 regular expression in Go's
// Perl syntax, without compiling it.
func checkPattern(re string) error {
	if len(re) > maxPatternBytes {
		return fmt.Errorf("a pattern is at most %d bytes", maxPatternBytes)
	}
	_, err := syntax.Parse(re, syntax.Perl)
	return err
}

// checkBranchName applies git's branch name rules (git check-ref-format
// --branch): a valid refs/heads/ ref that does not start with '-'.
func checkBranchName(name string) error {
	if strings.HasPrefix(name, "-") {
		return errors.New("a branch name does not start with '-'")
	}
	return checkRefName("refs/heads/" + name)
}

// checkRefName applies git's ref name rules (git check-ref-format) to a full
// ref name.
func checkRefName(ref string) error {
	switch {
	case strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "//"):
		return errors.New("a ref has no empty component")
	case strings.HasSuffix(ref, "."):
		return errors.New("a ref does not end with '.'")
	case strings.Contains(ref, ".."):
		return errors.New("a ref has no '..'")
	case strings.Contains(ref, "@{"):
		return errors.New("a ref has no '@{'")
	case ref == "@":
		return errors.New("'@' alone is not a ref")
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return fmt.Errorf("a ref has no %q", r)
		}
	}
	for _, c := range strings.Split(ref, "/") {
		if strings.HasPrefix(c, ".") || strings.HasSuffix(c, ".lock") {
			return fmt.Errorf("component %q starts with '.' or ends with '.lock'", c)
		}
	}
	return nil
}

// cents converts a schema-valid USD amount to cents.
func cents(usd string) (int64, error) {
	whole, frac, _ := strings.Cut(usd, ".")
	for len(frac) < 2 {
		frac += "0"
	}
	n, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q: %w", usd, err)
	}
	return n, nil
}
