// SPDX-License-Identifier: Apache-2.0

// Package evidence is the runtime side of evidence nodes (docs/design/m0/12)
// that is not the graph's. It declares the remote_process effect kind and
// checks an evidence node's restore against its instrument contract.
//
// Decisions:
//   - An evidence run is an attempt (D40). Its run id, the name a receipt
//     gives it (06 Demand.run), is the attempt's TypeID under the run_
//     prefix (05), so the pair needs no record: RunOf derives it, and a
//     remote_process request that names another run is refused.
//   - remote_process keeps its request (RequestBlob). The request must be a
//     records/remote-process-request, and it must name the intent's target
//     and the run of its attempt.
//   - The restore procedure is the instrument contract's. An evidence node's
//     restore is exactly the contract's: required with its procedure, or not
//     required when the contract has none.
package evidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// RemoteProcess is the effect kind of remote steps (12 "Remote processes").
const RemoteProcess = "remote_process"

// RemoteProcessKind declares remote_process: never retried, reconciled from
// its kept, validated request.
var RemoteProcessKind = eventstore.EffectKind{RequestBlob: true, RequestSchema: "records/remote-process-request", Check: checkRemoteProcess}

// RunOf is the run id of an evidence attempt (05).
func RunOf(attempt types.AttemptID) types.EvidenceRunID {
	return types.EvidenceRunID("run_" + strings.TrimPrefix(string(attempt), "att_"))
}

// checkRemoteProcess: the request, already a valid remote-process-request,
// names the intent's target and its attempt's run.
func checkRemoteProcess(target string, request json.RawMessage) error {
	var r types.RemoteProcessRequest
	if err := json.Unmarshal(request, &r); err != nil {
		return err
	}
	if string(r.Target) != target {
		return fmt.Errorf("evidence: the request's target %s is not the intent's %s", r.Target, target)
	}
	if r.Run != RunOf(r.Attempt) {
		return fmt.Errorf("evidence: run %s is not attempt %s's run", r.Run, r.Attempt)
	}
	return nil
}

// CheckRestore checks an evidence node's restore against its contract c.
func CheckRestore(p types.EvidenceNodeParams, c types.InstrumentContract) error {
	want := c.RestoreProcedure
	switch {
	case p.Restore.Required != (want != nil):
		return fmt.Errorf("evidence: the restore is required exactly when contract %s has a procedure", c.Name)
	case want != nil && (p.Restore.Procedure == nil || *p.Restore.Procedure != *want):
		return errors.New("evidence: the restore procedure is not the contract's")
	}
	return nil
}
