// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
)

// M1 P3: adcd registers task.created and its projector, but task.create
// stays deferred until P22a activates intake: the installed command, with
// a scope file and an attachment, is refused action_deferred and records
// nothing.
func TestTaskCreateStaysDeferred(t *testing.T) {
	c := runCommanded(t)
	op := c.op.Reveal()
	dir := t.TempDir()
	scope, accept := filepath.Join(dir, "spec.json"), filepath.Join(dir, "accept_test.go")
	body := `{"project":"prj_01m3250vkwffc979nzjw0g2sa1","title":"t","requirement":"r"}`
	if err := errors.Join(os.WriteFile(scope, []byte(body), 0o600), os.WriteFile(accept, []byte("package x\n"), 0o600)); err != nil {
		t.Fatal(err)
	}
	code, out, errb := c.adc(op, "task", "create", "--expect", "0", "--reason", "smoke", "--scope-file", scope, "--attach", accept)
	if code != entry.ExitCouldNotRun || !strings.Contains(out+errb, "action_deferred") {
		t.Fatalf("task create: %d %s %s", code, out, errb)
	}
	if _, ok := handlers()[authority.TaskCreateAction]; ok {
		t.Error("task.create has a handler in adcd")
	}
	kinds, err := eventKinds()
	if _, ok := kinds[authority.TaskCreated]; err != nil || !ok {
		t.Errorf("task.created is not registered: %v", err)
	}
}
