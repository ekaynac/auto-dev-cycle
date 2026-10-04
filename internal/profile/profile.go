// SPDX-License-Identifier: Apache-2.0

// Package profile loads and checks project profiles (docs/design/m0/07):
// the schema first, then the semantic checks the schema cannot express.
// Validation runs before adoption, never after.
//
// Two checks of note 07 need what M0 does not have yet and land with it:
// that the base ref exists (repository access) and that the data policy is
// within the installation's usage profile (the usage profiles, M2).
package profile

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

// Schema is the profile's contract schema.
const Schema = "config/project-profile"

// Load reads a profile (YAML or JSON), validates it against the schema and
// runs the semantic checks. Any problem refuses the whole profile.
func Load(raw []byte) (*types.ProjectProfile, error) {
	js, err := yamlToJSON(raw)
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

	var protected [][]string
	for _, g := range p.ProtectedPaths {
		segs, err := compileGlob(string(g))
		if err != nil {
			bad("protected_paths", "%q: %v", g, err)
			continue
		}
		protected = append(protected, segs)
	}
	covered := func(segs []string) bool {
		for _, q := range protected {
			if globMatch(q, segs) {
				return true
			}
		}
		return false
	}
	for _, g := range p.ProtectedDocuments {
		segs, err := compileGlob(string(g))
		if err != nil {
			bad("protected_documents", "%q: %v", g, err)
		} else if !covered(segs) {
			bad("protected_documents", "%q is not covered by protected_paths", g)
		}
	}
	if t := p.Integration.Topology; t != nil {
		for _, f := range t.Files {
			if checkRepoPath(string(f)) == nil && !covered(strings.Split(string(f), "/")) {
				bad("integration.topology.files", "%q is not covered by protected_paths (the topology is protected)", f)
			}
		}
	}

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
		if _, err := regexp.Compile(*re); err != nil {
			bad("vocabulary.task_id_pattern", "%v", err)
		}
	}
	return errors.Join(errs...)
}

// checkRepoPath: a relative, slash-separated, clean path inside the
// repository.
func checkRepoPath(p string) error {
	switch {
	case strings.ContainsRune(p, '\\'):
		return errors.New("use forward slashes")
	case strings.HasPrefix(p, "/"):
		return errors.New("must be relative to the repository root")
	case p == "." || path.Clean(p) != p:
		return fmt.Errorf("must be clean (%q)", path.Clean(p))
	case p == ".." || strings.HasPrefix(p, "../"):
		return errors.New("must stay inside the repository")
	}
	return nil
}

// compileGlob splits a repository glob into segments, each "**" (any number
// of segments) or a path.Match pattern.
func compileGlob(g string) ([]string, error) {
	if strings.ContainsRune(g, '\\') || strings.HasPrefix(g, "/") || path.Clean(g) != g {
		return nil, errors.New("a glob is relative, clean and slash-separated")
	}
	segs := strings.Split(g, "/")
	for _, s := range segs {
		switch {
		case s == "..":
			return nil, errors.New("a glob stays inside the repository")
		case s == "**":
		case strings.Contains(s, "**"):
			return nil, fmt.Errorf("%q: ** must be a whole segment", s)
		default:
			if _, err := path.Match(s, ""); err != nil {
				return nil, fmt.Errorf("%q: %w", s, err)
			}
		}
	}
	return segs, nil
}

// globMatch reports whether the glob segments match the path segments. A
// path segment may itself be a pattern (to check that one glob covers
// another); it then matches only a glob segment that is identical or
// matches it literally, or "**".
func globMatch(glob, segs []string) bool {
	if len(glob) == 0 {
		return len(segs) == 0
	}
	if glob[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if globMatch(glob[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 || segs[0] == "**" {
		return false
	}
	if ok, _ := path.Match(glob[0], segs[0]); !ok && glob[0] != segs[0] {
		return false
	}
	return globMatch(glob[1:], segs[1:])
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
