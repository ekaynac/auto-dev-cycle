// SPDX-License-Identifier: Apache-2.0

package vocab

import (
	"strings"
	"testing"
	"testing/fstest"
)

// The defaults are note 04's table (D32, K5, KA1, M12), exactly.
func TestChangeClassDefaults(t *testing.T) {
	type policy struct {
		confirmation string
		settable     bool
	}
	want := map[string]policy{
		"install.self":                        {"independent_review", true},
		"policy.widen_data":                   {"human", false},
		"policy.widen_destinations":           {"human", false},
		"policy.weaken_protection":            {"human", false},
		"capacity.allow_paid_fallback":        {"human", false},
		"agent.enrol":                         {"human", false},
		"grant.create":                        {"human", false},
		"grant.extend":                        {"human", false},
		"knowledge.accept_claim":              {"independent_review", true}, // K5: settable to human
		"knowledge.export_framework_prose":    {"human", false},
		"knowledge.change_protected_document": {"independent_review", true},
		"integration.change_topology":         {"independent_review", true},
	}
	reg := mustLoad(t)
	if len(reg.ChangeClasses) != len(want) {
		t.Fatalf("%d defaults, want %d", len(reg.ChangeClasses), len(want))
	}
	for class, p := range want {
		d, ok := reg.ChangeClasses[class]
		if !ok || string(d.Confirmation) != p.confirmation || d.SettableToHuman != p.settable {
			t.Errorf("%s: %+v, want %+v", class, d, p)
		}
		if d.EveryActorKind != (class == "knowledge.export_framework_prose") {
			t.Errorf("%s: every_actor_kind %v", class, d.EveryActorKind)
		}
	}
}

// The M0 action inventory and each action's CLI family (04, 08): an action
// dropped from the registry would drop its endpoint and parity mapping.
func TestActionInventory(t *testing.T) {
	want := map[string]string{
		"project.create": "project", "profile.adopt": "project", "task.create": "task", "task.cancel": "task", "task.raise_rail": "task", "review.request": "task",
		"input.answer": "holds", "proposal.confirm": "approve", "proposal.withdraw": "propose",
		"grant.create": "grant", "grant.extend": "grant", "grant.revoke": "grant",
		"agent.enrol": "admin", "actor.revoke": "admin", "token.issue": "admin", "token.revoke": "admin", "token.replace": "admin", "seat.take": "seat", "seat.release": "seat",
		"knowledge.change_protected_document": "project", "integration.change_topology": "project",
		"note.add": "notes",
	}
	got := mustLoad(t).Actions
	if len(got) != len(want) {
		t.Errorf("%d actions, want %d", len(got), len(want))
	}
	for action, family := range want {
		if a, ok := got[action]; !ok || string(a.Family) != family {
			t.Errorf("%s: %+v, want family %s", action, a, family)
		}
	}
}

func registryWith(actions string) fstest.MapFS {
	return fstest.MapFS{
		"vocab/outcomes.yaml": {Data: []byte(outcomesYAML)},
		"vocab/holds.yaml":    {Data: []byte(holdsYAML)},
		"vocab/reasons.yaml":  {Data: []byte(reasonsYAML)},
		"vocab/actions.yaml":  {Data: []byte(actions)},
	}
}

func TestActionsRefusals(t *testing.T) {
	line := func(prefix string) string {
		for _, l := range strings.SplitAfter(actionsYAML, "\n") {
			if strings.HasPrefix(l, prefix) {
				return l
			}
		}
		t.Fatalf("no line %q", prefix)
		return ""
	}
	first, install := line("  - {action: project.create,"), line("  - {class: install.self,")
	cases := map[string]struct{ edit, want string }{
		"a repeated action": {strings.Replace(actionsYAML, first, first+first, 1), "action project.create registered twice"},
		"a repeated class":  {strings.Replace(actionsYAML, install, install+install, 1), "change class install.self has two defaults"},
		"a missing class":   {strings.Replace(actionsYAML, install, "", 1), "change class install.self has no confirmation default"},
		"a settable human default": {strings.Replace(actionsYAML, "class: agent.enrol, confirmation: human, settable_to_human: false",
			"class: agent.enrol, confirmation: human, settable_to_human: true", 1), "contract vocab/registry"},
		"every actor kind without human": {strings.Replace(actionsYAML, install,
			strings.Replace(install, "every_actor_kind: false", "every_actor_kind: true", 1), 1), "contract vocab/registry"},
		"an undotted action": {strings.Replace(actionsYAML, "action: project.create,", "action: project,", 1), "contract vocab/registry"},
		"an unknown class":   {strings.Replace(actionsYAML, "class: install.self,", "class: install.anything,", 1), "contract vocab/registry"},
	}
	for name, c := range cases {
		if c.edit == actionsYAML {
			t.Fatalf("%s: the edit did not apply", name)
		}
		_, err := loadFrom(registryWith(c.edit), []string{"vocab/actions.yaml", "vocab/holds.yaml", "vocab/outcomes.yaml", "vocab/reasons.yaml"})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
}
