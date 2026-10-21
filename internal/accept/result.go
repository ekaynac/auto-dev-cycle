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

// Report bounds: entries per list, bytes per field (a path or a what),
// and encoded bytes of all entries together.
const (
	maxReported = 1000
	maxField    = 1024
	maxEntries  = 1 << 20
)

var reported, reportedBytes = maxReported, maxEntries // tests lower them

// Report is the D55 record of one accept node, a blob its result holds
// from the first live attempt: elapsed time, blocks, warnings, files,
// tree. Each list keeps at most maxReported entries, and all entries
// together at most maxEntries encoded bytes; the entries left out are
// counted in truncated, and the fields cut to maxField bytes in clipped,
// those of the entries the guard left out (omitted) too.
func (v Verdict) Report() ([]byte, error) {
	_, out, truncated, clipped, err := selection([2][]Block{v.Blocks, v.Warnings})
	if err != nil {
		return nil, err
	}
	for _, o := range v.omitted {
		truncated, clipped = truncated+o.Entries, clipped+o.Clipped
	}
	blocks, warnings := out[0], out[1]
	return json.Marshal(struct {
		Schema     string       `json:"schema"`
		ElapsedMS  int64        `json:"elapsed_ms"`
		Blocks     []Block      `json:"blocks"`
		Warnings   []Block      `json:"warnings"`
		Truncated  int          `json:"truncated"`
		Clipped    int          `json:"clipped"`
		Files      int          `json:"files"`
		Tree       string       `json:"tree"`
		TreeDigest types.Digest `json:"tree_digest"`
	}{reportSchema, v.Elapsed.Milliseconds(), blocks, warnings, truncated, clipped, v.Files, v.Tree, v.TreeDigest})
}

// selection is the report's choice of blocks (lists[0]) and warnings
// (lists[1]): the base guard's findings first, P14b-out's text ones, then
// its types check's alone (Astra #282 r2 H1), each list in turn in what is
// left; in, which entries it keeps, the guard's process too (fitReport).
func selection(lists [2][]Block) (in [2][]bool, out [2][]Block, truncated, clipped int, err error) {
	budget := reportedBytes
	for l := range 2 {
		in[l], out[l] = make([]bool, len(lists[l])), []Block{}
	}
	for rank := range 3 {
		for l := range 2 {
			for i, b := range lists[l] {
				if b2i(b.Additive)+b2i(b.Typed) != rank {
					continue
				}
				for _, f := range []*string{&b.Path, &b.What} {
					if len(*f) > maxField {
						*f, clipped = strings.ToValidUTF8((*f)[:maxField], ""), clipped+1
					}
				}
				e, merr := json.Marshal(b)
				err = errors.Join(err, merr)
				if len(out[l]) == reported || len(e)+1 > budget {
					truncated++
					continue
				}
				budget -= len(e) + 1
				out[l], in[l][i] = append(out[l], b), true
			}
		}
	}
	return in, out, truncated, clipped, err
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
		r.Outcome, r.Reason, r.Detail = types.OutcomeFailed, v.Reason(), detail("accept refused the candidate", v.Blocks, v.omitted[0].Entries)
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
		r.Outcome, r.Reason, r.Detail = types.OutcomeFound, vocab.ReasonCompletedWithFindings, detail("accepted, with warnings for the reviewer", v.Warnings, v.omitted[1].Entries)
	}
	return r, nil
}

// detail lists findings within the result's detail bound; paths are the
// candidate's, so each is quoted. omitted entries, left out after bs's,
// are among the "more" (an omission keeps a list's first detailKept
// entries, which pass the bound).
func detail(head string, bs []Block, omitted int) string {
	var b strings.Builder
	b.WriteString(head)
	for i, x := range bs {
		line := fmt.Sprintf("; %s %q: %s", x.Reason, x.Path, x.What)
		if b.Len()+len(line) > 4000 {
			fmt.Fprintf(&b, "; and %d more", len(bs)-i+omitted)
			break
		}
		b.WriteString(line)
	}
	return b.String()
}
