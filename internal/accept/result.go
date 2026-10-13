// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// reportSchema names the report's shape: P30's gate ledger reads it.
const reportSchema = "adc.accept-report/v1"

// maxReported bounds each of the report's lists; the rest are counted.
const maxReported = 1000

// Report is the D55 record of one accept node, a blob its result holds
// from the first live attempt: elapsed time, blocks, warnings (each list
// at most maxReported, the rest counted in truncated), files, tree.
func (v Verdict) Report() ([]byte, error) {
	cut := func(bs []Block) []Block { return append([]Block{}, bs[:min(len(bs), maxReported)]...) }
	return json.Marshal(struct {
		Schema     string       `json:"schema"`
		ElapsedMS  int64        `json:"elapsed_ms"`
		Blocks     []Block      `json:"blocks"`
		Warnings   []Block      `json:"warnings"`
		Truncated  int          `json:"truncated"`
		Files      int          `json:"files"`
		Tree       string       `json:"tree"`
		TreeDigest types.Digest `json:"tree_digest"`
	}{reportSchema, v.Elapsed.Milliseconds(), cut(v.Blocks), cut(v.Warnings),
		len(v.Blocks) + len(v.Warnings) - len(cut(v.Blocks)) - len(cut(v.Warnings)), v.Files, v.Tree, v.TreeDigest})
}

// Result is the accept node's completed result for key, kind tree.
// Accepted, it holds the tree, then the report: empty, or found with the
// guard's warnings. Refused, it holds the report and fails.
func Result(key types.ComputationKey, v Verdict, project types.ProjectID, report types.Digest) (types.Result, error) {
	id, err := ids.New("res")
	if err != nil {
		return types.Result{}, err
	}
	seal := func(t types.RefType, id string, d types.Digest) (types.Ref, error) {
		return knowledge.Seal(types.Ref{Type: t, ID: id, Scope: types.RefScope{Project: &project},
			Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: d})
	}
	rep, err := seal(types.RefTypeBlob, string(report), report)
	if err != nil {
		return types.Result{}, err
	}
	r := types.Result{ID: types.ResultID(id), Computation: key.Key, Kind: types.ResultKindTree, Value: []types.Ref{rep}, Completed: true}
	switch {
	case !v.Accepted():
		r.Outcome, r.Reason, r.Detail = types.OutcomeFailed, v.Reason(), detail("accept refused the candidate", v.Blocks)
		return r, nil
	case v.TreeDigest == "":
		return types.Result{}, errors.New("accept: an accepted verdict without its tree")
	}
	tree, err := seal(types.RefTypeGitTree, v.Tree, v.TreeDigest)
	if err != nil {
		return types.Result{}, err
	}
	r.Value = []types.Ref{tree, rep}
	r.Outcome, r.Reason = types.OutcomeEmpty, vocab.ReasonCompletedClean
	if len(v.Warnings) > 0 {
		r.Outcome, r.Reason, r.Detail = types.OutcomeFound, vocab.ReasonCompletedWithFindings, detail("accepted, with warnings for the reviewer", v.Warnings)
	}
	return r, nil
}

// detail lists findings within the result's detail bound; paths are the
// candidate's, so each is quoted.
func detail(head string, bs []Block) string {
	var b strings.Builder
	b.WriteString(head)
	for i, x := range bs {
		line := fmt.Sprintf("; %s %q: %s", x.Reason, x.Path, x.What)
		if b.Len()+len(line) > 4000 {
			fmt.Fprintf(&b, "; and %d more", len(bs)-i)
			break
		}
		b.WriteString(line)
	}
	return b.String()
}
