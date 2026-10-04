// SPDX-License-Identifier: Apache-2.0

package vocab

import (
	"strings"
	"testing"
	"testing/fstest"
)

// The defaults are note 04's table (D32, K5, KA1, M12), exactly.
func TestChangeClassDefaults(t *testing.T) {
	want := map[string]string{
		"install.self":                        "independent_review",
		"policy.widen_data":                   "human",
		"policy.widen_destinations":           "human",
		"policy.weaken_protection":            "human",
		"capacity.allow_paid_fallback":        "human",
		"agent.enrol":                         "human",
		"grant.create":                        "human",
		"grant.extend":                        "human",
		"knowledge.accept_claim":              "independent_review",
		"knowledge.export_framework_prose":    "human",
		"knowledge.change_protected_document": "independent_review",
		"integration.change_topology":         "independent_review",
	}
	reg := mustLoad(t)
	if len(reg.ChangeClasses) != len(want) {
		t.Fatalf("%d defaults, want %d", len(reg.ChangeClasses), len(want))
	}
	for class, policy := range want {
		d, ok := reg.ChangeClasses[class]
		if !ok || string(d.Confirmation) != policy {
			t.Errorf("%s: %+v, want %s", class, d, policy)
		}
		if d.EveryActorKind != (class == "knowledge.export_framework_prose") {
			t.Errorf("%s: every_actor_kind %v", class, d.EveryActorKind)
		}
	}
	if len(reg.Actions) == 0 {
		t.Fatal("no actions")
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
