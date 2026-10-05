// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// decided records an authority decision as its step: allowed is
// empty/completed_clean, refused is failed/<reason>. A proposal has no
// outcome (02 "Disposition is not an outcome"): where a fixture expects
// none, it is a violation, never a step.
func (o *observe) decided(d authority.Decision) {
	switch d.Disposition {
	case authority.Allow:
		o.steps = append(o.steps, Step{vocab.Empty, vocab.ReasonCompletedClean.String()})
	case authority.Refuse:
		o.steps = append(o.steps, Step{vocab.Failed, d.Reason.String()})
	default:
		o.check(false, "the next change was proposed under %s, not decided", d.Policy)
	}
}

// adopted is a project's adopted profile, with the policy-relevant fields
// set; the classifier reads nothing else.
func adopted() types.ProjectProfile {
	std := types.ProjectProfileUsageProfileOverrideValueStandard
	return types.ProjectProfile{
		DataPolicy: types.ProjectProfileDataPolicy{Floor: types.ProjectProfileDataPolicyFloorContracted,
			Routing: types.ProjectProfileDataPolicyRoutingVerified, AllowedDestinations: []types.Slug{"anthropic", "openai"}},
		UsageProfileOverride: &std,
		ProtectedPaths:       []types.Glob{"docs/decisions/**", "CANON.md"},
		ProtectedDocuments:   []types.Glob{"docs/decisions/*.md"},
	}
}

// edited is the adopted profile with one edit.
func edited(edit func(*types.ProjectProfile)) types.ProjectProfile {
	p := adopted()
	edit(&p)
	return p
}

// actors are a supervisor, granted the actions under test on project A,
// and an operator and the system, which act unrestricted.
func actors(actions ...types.ActionKind) (sup, op, sys types.Actor, g types.Grant, err error) {
	var idv [4]string
	for i, p := range []string{"act", "act", "act", "grt"} {
		if idv[i], err = ids.New(p); err != nil {
			return
		}
	}
	op = types.Actor{ID: types.ActorID(idv[1]), Kind: types.ActorKindOperator, Name: "operator", CreatedBy: types.ActorID(idv[1])}
	sup = types.Actor{ID: types.ActorID(idv[0]), Kind: types.ActorKindSupervisor, Name: "supervisor", CreatedBy: op.ID}
	sys = types.Actor{ID: types.ActorID(idv[2]), Kind: types.ActorKindSystem, Name: "system", CreatedBy: op.ID}
	g = types.Grant{ID: types.GrantID(idv[3]), Actor: sup.ID, Projects: types.ProjectScope{List: []types.ProjectID{projectA}}, Actions: actions,
		ValidFrom: "2026-10-04T00:00:00Z", ExpiresAt: "2026-10-11T00:00:00Z",
		ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: op.ID}
	return
}

// widenedProfileEdit: a profile edit is classified by its effective change,
// never by its name. A narrowing edit is allowed; one that widens data
// (explicitly, or disguised as dropping the usage override, inheriting the
// installation's usage, which may be anything), widens destinations
// (adding one, or emptying the list), removes a protected document, or
// bundles a widening with a topology change is
// proposed for a presence-verified human, so a review GO for it is refused (11 "an effective-change widening
// disguised as a profile edit"; 04 "Action registry and effective-change
// classification").
func widenedProfileEdit(context.Context, string) (steps []Step, violations []string, err error) {
	reg, err := vocab.Load()
	if err != nil {
		return nil, nil, err
	}
	sup, _, _, g, err := actors("profile.adopt")
	if err != nil {
		return nil, nil, err
	}
	cur, project := adopted(), projectA
	var o observe
	// Each edit with the classes its effective change must carry (none for
	// the narrowing one). The last bundles a wider floor with a topology
	// change, which must not hide the widening.
	topology := &types.ProjectProfileIntegrationTopologyValue{Files: []types.RepoPath{"compose.yaml"},
		AdoptedRevision: types.GitID("git:" + strings.Repeat("c", 40)), Digest: types.Digest("sha256:" + strings.Repeat("a", 64))}
	for _, c := range []struct {
		edit    func(*types.ProjectProfile)
		classes []string
	}{
		{func(p *types.ProjectProfile) { p.DataPolicy.AllowedDestinations = []types.Slug{"anthropic"} }, nil},
		{func(p *types.ProjectProfile) { p.DataPolicy.Floor = types.ProjectProfileDataPolicyFloorFreeUnknown }, []string{authority.WidenData}},
		{func(p *types.ProjectProfile) { p.UsageProfileOverride = nil }, []string{authority.WidenData}},
		{func(p *types.ProjectProfile) {
			p.DataPolicy.AllowedDestinations = append(p.DataPolicy.AllowedDestinations, "deepseek")
		}, []string{authority.WidenDestinations}},
		{func(p *types.ProjectProfile) { p.DataPolicy.AllowedDestinations = []types.Slug{} }, []string{authority.WidenDestinations}},
		{func(p *types.ProjectProfile) { p.ProtectedDocuments = []types.Glob{} }, []string{authority.WeakenProtection}},
		{func(p *types.ProjectProfile) {
			p.Integration.Topology = topology
			p.DataPolicy.Floor = types.ProjectProfileDataPolicyFloorFreeUnknown
		}, []string{authority.ChangeTopology, authority.WidenData}},
	} {
		d := authority.Decide(reg, authority.Request{Actor: sup, Action: "profile.adopt", Project: &project,
			Classes: authority.ClassifyProfile(&cur, edited(c.edit)), Grants: []types.Grant{g}, Now: receiptsAt})
		if d.Disposition != authority.Propose {
			o.decided(d)
			continue
		}
		o.check(d.Policy == authority.Human && !slices.ContainsFunc(c.classes, func(k string) bool { return !slices.Contains(d.Classes, k) }),
			"the next edit is proposed under %s for %v, want human for %v", d.Policy, d.Classes, c.classes)
		prp, err := ids.New("prp")
		if err != nil {
			return nil, nil, err
		}
		p, err := authority.NewProposal(types.ProposalID(prp), d, authority.Bound{Action: "profile.adopt", Project: &project,
			Object: types.ObjectRef{Type: "profile", ID: string(project)}, Scope: json.RawMessage(`{}`), ProposedBy: sup.ID}, receiptsAt.Add(time.Hour))
		if err != nil {
			return nil, nil, err
		}
		r, err := newReceipt(types.ReceiptKindReview, reviewPanel(), proposalSubject(p.ID, p.BindingDigest), []string{"claude"},
			pred("findings", vocab.Empty, vocab.ReasonCompletedClean))
		if err == nil {
			r, err = recorded(reviewPanel(), r, nil)
		}
		if err != nil {
			return nil, nil, err
		}
		_, err = authority.ConfirmByReview(p, r, reviewPanel(), nil, []string{"gpt"}, receiptsAt)
		if err := o.refused(err); err != nil {
			return nil, nil, err
		}
	}
	return o.steps, o.violations, nil
}

// protectedChangeWidens: a protected-document change whose new content
// widens policy is refused for every actor, under any confirmation; one
// that widens nothing is allowed for an operator and proposed for
// independent review for a supervisor (11 "a protected-document change carrying
// a policy widening"; 04 "Protected changes").
func protectedChangeWidens(context.Context, string) (steps []Step, violations []string, err error) {
	reg, err := vocab.Load()
	if err != nil {
		return nil, nil, err
	}
	const action = "knowledge.change_protected_document"
	sup, op, sys, g, err := actors(action)
	if err != nil {
		return nil, nil, err
	}
	cur, project := adopted(), projectA
	var o observe
	for _, c := range []struct {
		by   types.Actor
		edit func(*types.ProjectProfile)
	}{
		{op, func(p *types.ProjectProfile) { p.ProtectedPaths = append(p.ProtectedPaths, "deploy/**") }},
		{op, func(p *types.ProjectProfile) {
			p.DataPolicy.AllowedDestinations = append(p.DataPolicy.AllowedDestinations, "deepseek")
		}},
		{sys, func(p *types.ProjectProfile) { p.ProtectedPaths = []types.Glob{"CANON.md"} }},
		{sup, func(p *types.ProjectProfile) { p.DataPolicy.Routing = types.ProjectProfileDataPolicyRoutingReportedOk }},
	} {
		o.decided(authority.Decide(reg, authority.Request{Actor: c.by, Action: action, Project: &project,
			Classes: authority.ClassifyProfile(&cur, edited(c.edit)), Grants: []types.Grant{g}, Grantee: &op.ID, Now: receiptsAt}))
	}
	// A supervisor's benign change is proposed for independent review: a
	// disposition, not an outcome, so an assertion rather than a step.
	d := authority.Decide(reg, authority.Request{Actor: sup, Action: action, Project: &project,
		Classes: authority.ClassifyProfile(&cur, edited(func(p *types.ProjectProfile) { p.ProtectedPaths = append(p.ProtectedPaths, "deploy/**") })),
		Grants:  []types.Grant{g}, Now: receiptsAt})
	o.check(d.Disposition == authority.Propose && d.Policy == authority.IndependentReview,
		"a supervisor's benign protected change is %+v, want a proposal for independent review", d)
	return o.steps, o.violations, nil
}
