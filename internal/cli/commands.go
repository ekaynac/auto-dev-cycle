// SPDX-License-Identifier: Apache-2.0

package cli

// commands maps every action of the registry (schemas/vocab/actions.yaml) to
// its one CLI command, `adc <noun> <verb>`, whose noun is the action's
// family (docs/design/m0/08 "CLI rules"). A test fails on an action with no
// command, a command with no action, or a noun that is not the family; the
// dashboard side of the parity check joins it in M5.
var commands = map[string]command{
	"project.create":                      {"project", "create"},
	"profile.adopt":                       {"project", "adopt-profile"},
	"knowledge.change_protected_document": {"project", "change-protected-document"},
	"integration.change_topology":         {"project", "change-topology"},
	"task.create":                         {"task", "create"},
	"review.request":                      {"task", "request-review"},
	"input.answer":                        {"holds", "answer"},
	"proposal.confirm":                    {"approve", "confirm"},
	"proposal.withdraw":                   {"propose", "withdraw"},
	"grant.create":                        {"grant", "create"},
	"grant.extend":                        {"grant", "extend"},
	"grant.revoke":                        {"grant", "revoke"},
	"agent.enrol":                         {"admin", "enrol-agent"},
	"actor.revoke":                        {"admin", "revoke-actor"},
	"seat.take":                           {"seat", "take"},
	"seat.release":                        {"seat", "release"},
	"note.add":                            {"notes", "add"},
}

type command struct{ noun, verb string }

// actionFor returns the action a noun and verb name.
func actionFor(noun, verb string) (string, bool) {
	for action, c := range commands {
		if c.noun == noun && c.verb == verb {
			return action, true
		}
	}
	return "", false
}

// isNoun reports whether some action command uses noun.
func isNoun(noun string) bool {
	for _, c := range commands {
		if c.noun == noun {
			return true
		}
	}
	return false
}
