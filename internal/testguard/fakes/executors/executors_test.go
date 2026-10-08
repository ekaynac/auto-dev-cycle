// SPDX-License-Identifier: Apache-2.0

package executors

import (
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

func TestFamily(t *testing.T) {
	in := map[types.ExecutorID]string{"exe-claude": "claude", "exe-gpt": "gpt", "exe-blank": ""}
	r := New(in)
	in["exe-claude"], in["exe-new"] = "gpt", "gpt" // the caller's map is not the registry
	for id, want := range map[types.ExecutorID]string{
		"exe-claude": "claude",
		"exe-gpt":    "gpt",
		"exe-blank":  Unknown, // a registered executor with no family recorded
		"exe-new":    Unknown, // added to the caller's map after New
		"exe-absent": Unknown,
	} {
		if got := r.Family(id); got != want {
			t.Errorf("Family(%s) = %q, want %q", id, got, want)
		}
	}
	if got := (Registry{}).Family("exe-claude"); got != Unknown {
		t.Errorf("an empty registry gives %q, want %q", got, Unknown)
	}
}
