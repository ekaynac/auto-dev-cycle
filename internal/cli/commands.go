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
	"task.cancel":                         {"task", "cancel"},
	"review.request":                      {"task", "request-review"},
	"input.answer":                        {"holds", "answer"},
	"proposal.confirm":                    {"approve", "confirm"},
	"proposal.withdraw":                   {"propose", "withdraw"},
	"grant.create":                        {"grant", "create"},
	"grant.extend":                        {"grant", "extend"},
	"grant.revoke":                        {"grant", "revoke"},
	"agent.enrol":                         {"admin", "enrol-agent"},
	"actor.revoke":                        {"admin", "revoke-actor"},
	"token.issue":                         {"admin", "issue-token"},
	"token.revoke":                        {"admin", "revoke-token"},
	"token.replace":                       {"admin", "replace-token"},
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

// Commands maps every action the CLI carries to its command line,
// "adc <noun> <verb>" (the parity harness, internal/parity, joins it with
// the daemon's wiring and the API description).
func Commands() map[string]string {
	out := make(map[string]string, len(commands))
	for action, c := range commands {
		out[action] = "adc " + c.noun + " " + c.verb
	}
	return out
}

// ReadCommands maps every read the CLI carries to its command line, by
// resource: those of reads.go, "adc <noun> <verb>", and the event stream
// (events.go) and the board, which Main routes by their first word.
func ReadCommands() map[string]string {
	out := map[string]string{"events": "adc events --follow", "board": "adc board"}
	for c, resource := range reads {
		out[resource] = "adc " + c.noun + " " + c.verb
	}
	return out
}
