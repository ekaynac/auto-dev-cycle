// SPDX-License-Identifier: Apache-2.0

package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

const (
	tgt = "tgt_01m3250v0aer9ss2097svmefj8"
	att = "att_01m3250v57fhm9v1z9xd1wk8gw"
)

func TestRunOf(t *testing.T) {
	if got := RunOf(att); got != "run_01m3250v57fhm9v1z9xd1wk8gw" {
		t.Fatalf("RunOf = %s", got)
	}
}

// A remote_process intent is admitted only with a kept request that is a
// valid remote-process-request naming the intent's target and its
// attempt's run.
func TestRemoteProcessRequests(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	lock, err := eventstore.AcquireLock(state)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	allow := func(*eventstore.Tx, eventstore.Command) error { return nil }
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: eventstore.Kinds{}, Blobs: blobs,
		Hooks: eventstore.Hooks{Identify: allow, Authorize: allow}, Effects: eventstore.EffectKinds{RemoteProcess: RemoteProcessKind}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); lock.Release() })
	request := func(edit func(map[string]any)) json.RawMessage {
		r := map[string]any{"target": tgt, "attempt": att, "run": string(RunOf(att)), "identity": "lab-admin",
			"sessions": []string{"s-1"}, "deadline_at": "2026-10-08T12:30:00Z"}
		edit(r)
		b, _ := json.Marshal(r) // encoding/json sorts keys: the RFC 8785 form of this request
		if _, err := blobs.Put(bytes.NewReader(b), ""); err != nil {
			t.Fatal(err)
		}
		return b
	}
	var eff string
	intend := func(target string, req json.RawMessage) error {
		cmd, _ := ids.New("cmd")
		eff, _ = ids.New("eff")
		actor, _ := ids.New("act")
		_, err := s.SubmitApplied(context.Background(), eventstore.Command{ID: cmd, Actor: actor, Action: "evidence.test", IdempotencyKey: cmd, Input: []byte(`{}`)},
			eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				return nil, json.RawMessage(`{}`), tx.IntendEffect(eff, RemoteProcess, target, req)
			}))
		return err
	}
	if err := intend(tgt, request(func(map[string]any) {})); err != nil {
		t.Fatalf("a valid request: %v", err)
	}
	// The real declaration is not idempotent: an uncertain remote_process is
	// reconciled, never retried, and a refused retry leaves it uncertain.
	ctx, sys, id := context.Background(), "act_01m3250vm7f6ktw39dszqnyddr", eff
	if _, err := s.RecordEffect(ctx, sys, id, eventstore.Transition{Kind: "uncertain", Observation: "timeout"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEffect(ctx, sys, id, eventstore.Transition{Kind: "retried"}); !errors.Is(err, eventstore.ErrEffectTransition) {
		t.Fatalf("a remote_process retried: %v", err)
	}
	if e, err := s.LoadEffect(ctx, id); err != nil || e.State != eventstore.EffectUncertain {
		t.Fatalf("after the refused retry: %+v (%v)", e, err)
	}
	unkept := json.RawMessage(strings.Replace(string(request(func(map[string]any) {})), `"s-1"`, `"s-2"`, 1))
	if err := intend(tgt, unkept); err == nil || !strings.Contains(err.Error(), "is not a published blob") {
		t.Errorf("an unkept request: %v", err)
	}
	for name, c := range map[string]struct {
		target string
		edit   func(map[string]any)
		want   string
	}{
		"another target":    {"tgt_01m3250vz2ezn81cgvbkcn17jd", func(map[string]any) {}, "is not the intent's"},
		"another run":       {tgt, func(r map[string]any) { r["run"] = "run_01m3250vz2ezn81cgvbkcn17jd" }, "is not attempt"},
		"not a request":     {tgt, func(r map[string]any) { delete(r, "sessions") }, "remote-process-request"},
		"a principal":       {tgt, func(r map[string]any) { r["identity"] = "Administrator@LAB" }, "remote-process-request"},
		"no session":        {tgt, func(r map[string]any) { r["sessions"] = []string{} }, "remote-process-request"},
		"a run for attempt": {tgt, func(r map[string]any) { r["attempt"] = string(RunOf(att)) }, "remote-process-request"},
	} {
		if err := intend(c.target, request(c.edit)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheckRestore(t *testing.T) {
	proc := types.Name("snapshot-revert")
	other := types.Name("reimage")
	with := types.InstrumentContract{Name: "walk", RestoreProcedure: &proc}
	without := types.InstrumentContract{Name: "probe"}
	restore := func(required bool, p *types.Name) types.EvidenceNodeParams {
		var e types.EvidenceNodeParams
		e.Restore.Required, e.Restore.Procedure = required, p
		return e
	}
	for name, c := range map[string]struct {
		p  types.EvidenceNodeParams
		c  types.InstrumentContract
		ok bool
	}{
		"the contract's procedure":          {restore(true, &proc), with, true},
		"no restore, none in the contract":  {restore(false, nil), without, true},
		"another procedure":                 {restore(true, &other), with, false},
		"a restore the contract has not":    {restore(true, &proc), without, false},
		"skipping the contract's restore":   {restore(false, nil), with, false},
		"required with no procedure (read)": {restore(true, nil), with, false},
	} {
		if err := CheckRestore(c.p, c.c); (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}
