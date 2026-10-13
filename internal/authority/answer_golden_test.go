// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// Frozen owed_input.recorded/v1 and input.answer approval.recorded/v1
// payloads replay, an operator's and a supervisor's under its grant
// (created first): the answer fold reads only the scope's owed input and
// revision, as recorded, so neither another reason nor a scope today's
// schema would refuse stops a stored answer from replaying.
func TestAnswerGoldensReplay(t *testing.T) {
	owed, err1 := os.ReadFile("testdata/owed-input-recorded-v1.json")
	opApr, err2 := os.ReadFile("testdata/approval-recorded-answer-v1.json")
	supApr, err3 := os.ReadFile("testdata/approval-recorded-answer-supervisor-v1.json")
	if err := errors.Join(err1, err2, err3); err != nil {
		t.Fatal(err)
	}
	const ownID = "own_01m3250vkwffc979nzjw0gwnn1"
	goldens := map[string]struct {
		apr        []byte
		id         string
		supervised bool
	}{
		"operator":   {opApr, "apr_01m3250vkwffc979nzjw0gans1", false},
		"supervisor": {supApr, "apr_01m3250vkwffc979nzjw0gans2", true},
	}
	edits := map[string][2]string{
		"as frozen":                            {},
		"another reason":                       {"input.answer answers the owed input at the revision it binds", "answered before the wording changed"},
		"a scope field today's schema refuses": {`"owed_revision": 1,`, `"owed_revision": 1, "note": "a field added later",`},
	}
	for who, golden := range goldens {
		for name, edit := range edits {
			t.Run(who+"/"+name, func(t *testing.T) {
				b := openAnswers(t)
				by, grant := b.op, types.GrantID("")
				if golden.supervised {
					g := b.supGrant
					g.ID, g.Actions = types.GrantID(newID(t, "grt")), []types.ActionKind{authority.InputAnswerAction}
					b.seed(b.op, createGrant(g))
					by, grant = b.sup, g.ID
				}
				b.seed(by, goldenAnswer(t, b, by, grant, owed, golden.apr, edit))
				if got, ok := b.answeredOf(ownID, 1); !ok || string(got) != golden.id {
					t.Fatalf("the golden answer: %s %t", got, ok)
				}
				b.rebuilds()
			})
		}
	}
}

// goldenAnswer records the owed input golden, owned by by, and the answer
// golden by by under grant, with edit applied.
func goldenAnswer(t *testing.T, b *answers, by identity.Principal, grant types.GrantID, owed, apr []byte, edit [2]string) applyFn {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		fill := strings.NewReplacer("@ACTOR@", string(by.Actor()), "@AT@", tx.Now().Format(time.RFC3339Nano), "@NODE@", string(b.asker),
			"@GRANT@", string(grant))
		a := fill.Replace(string(apr))
		if edit[0] != "" {
			a = strings.Replace(a, edit[0], edit[1], 1)
		}
		var id struct{ ID string }
		o := fill.Replace(string(owed))
		p := string(prjA)
		if err := json.Unmarshal([]byte(a), &id); err != nil {
			return nil, err
		}
		return []eventstore.Event{
			{ID: newID(t, "evt"), Kind: authority.OwedInputRecorded, Subject: "own_01m3250vkwffc979nzjw0gwnn1", Project: &p, Payload: []byte(o)},
			{ID: newID(t, "evt"), Kind: authority.ApprovalRecorded, Subject: id.ID, Project: &p, Payload: []byte(a)},
		}, nil
	}
}
