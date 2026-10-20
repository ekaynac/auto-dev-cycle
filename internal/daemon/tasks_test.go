// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

// appliedResult is the result of an applied command's --json response.
func appliedResult[T any](t *testing.T, what string, code int, out, errb string) (types.CommandApplied, T) {
	t.Helper()
	var res T
	a, ok := response(t, out).(*types.CommandApplied)
	if code != entry.ExitOK || !ok {
		t.Fatalf("%s: %d %s %s", what, code, out, errb)
	}
	if err := json.Unmarshal(a.Result, &res); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return *a, res
}

// P22a-2 (W9, W10): intake is active, so the installed commands create a
// project (P1's creation smoke) and a task in it, with a scope file and an
// attachment (P3's), which the board shows in Ready; the operator cancels
// it, its reason taken from --reason (the scope names none; Opus #298 L5),
// and the board shows it in Done, cancelled.
func TestIntake(t *testing.T) {
	c := runCommanded(t)
	op := c.op.Reveal()
	profile, err := os.ReadFile("../../fixtures/m1/profiles/py-mini.json")
	if err != nil {
		t.Fatal(err)
	}
	code, out, errb := c.adc(op, "project", "create", "--expect", "0", "--reason", "r", "--json", "--scope", string(profile))
	_, prj := appliedResult[struct{ Project types.ProjectID }](t, "project create", code, out, errb)
	code, out, errb = c.adc(op, "project", "list", "--json")
	if code != entry.ExitOK || contract.Validate("api/read-list", []byte(out)) != nil || !strings.Contains(out, `"name":"py-mini"`) {
		t.Fatalf("project list: %d %s %s", code, out, errb)
	}
	dir := t.TempDir()
	scope, accept := filepath.Join(dir, "spec.json"), filepath.Join(dir, "accept_test.py")
	body := fmt.Sprintf(`{"project":%q,"title":"t","requirement":"r"}`, prj.Project)
	if err := errors.Join(os.WriteFile(scope, []byte(body), 0o600), os.WriteFile(accept, []byte("def test_x(): pass\n"), 0o600)); err != nil {
		t.Fatal(err)
	}
	code, out, errb = c.adc(op, "task", "create", "--expect", "0", "--reason", "smoke", "--json", "--scope-file", scope, "--attach", accept)
	created, tsk := appliedResult[struct {
		Task  types.TaskID
		State string
	}](t, "task create", code, out, errb)
	if tsk.State != authority.TaskReady || created.Approval == nil {
		t.Fatalf("task create: %+v, approval %v", tsk, created.Approval)
	}
	// Its request layer dropped the upload the task published (P3).
	if code, out, errb = c.adc(op, "dev", "blob", "uploads", "--json"); code != entry.ExitOK || !strings.Contains(out, `"outcome":"empty"`) {
		t.Fatalf("the uploads after the task: %d %s %s", code, out, errb)
	}
	if code, out, errb = c.adc(op, "board", "--json"); code != entry.ExitOK || !strings.Contains(out, `"column":"ready"`) || !strings.Contains(out, `"cancelled":false`) {
		t.Fatalf("board: %d %s %s", code, out, errb)
	}
	cancel := fmt.Sprintf(`{"task":%q}`, tsk.Task)
	code, out, errb = c.adc(op, "task", "cancel", "--expect", fmt.Sprint(created.FirstSeq), "--reason", "smoke", "--json", "--scope", cancel)
	if _, res := appliedResult[struct{ State string }](t, "task cancel", code, out, errb); res.State != authority.TaskCancelledState {
		t.Fatalf("task cancel: %+v", res)
	}
	if code, out, errb = c.adc(op, "board", "--json"); code != entry.ExitOK || !strings.Contains(out, `"column":"done"`) || !strings.Contains(out, `"cancelled":true`) {
		t.Fatalf("board after the cancel: %d %s %s", code, out, errb)
	}
}
