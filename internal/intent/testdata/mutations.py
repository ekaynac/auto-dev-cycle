# SPDX-License-Identifier: Apache-2.0
#
# 22a-6L's mutations (W17's library-level parts: #226 entries 91, 94(2);
# the Opus #321 answers: H1, H2, M1, M2, L1-L5):
# each is applied alone to a clean copy of the repository, and the tests
# of intent, graph, gate, review and fixtures must fail on it (KILLED).
# Run from the repository root:
#
#   python3 internal/intent/testdata/mutations.py [jobs] [index ...]
#
# The copies keep symlinks (the adapters' node_modules).
import os
import shutil
import subprocess
import sys
import tempfile
from concurrent.futures import ThreadPoolExecutor

I, G, Q, R = 'internal/intent/intent.go', 'internal/graph/records.go', 'internal/gate/receipt.go', 'internal/review/review.go'
L, GG = 'internal/graph/lifecycle.go', 'internal/gate/gate.go'
MUTS = [
    # The bounded decode, the task's own (Opus #321 M1, L5).
    ("a result past its bound decoded", G, 'size > int64(maxResultBytes) || read > int64(maxResultsBytes)', 'read > int64(maxResultsBytes)'),
    ("the results past their bound decoded", G, 'size > int64(maxResultBytes) || read > int64(maxResultsBytes)', 'size > int64(maxResultBytes)'),
    ("another task's results read", G, 'tx.Keys(TaskResultProjection, prefix)', 'tx.Keys(TaskResultProjection, "")'),
    ("a node record read unbounded", G, '!ok || size > int64(maxNodeBytes)', '!ok'),
    ("the task's result index not written", L, 'if err := put(tx, TaskResultProjection, string(rec.Recorded.Task)+"/"+string(p.Result.ID), true); err != nil {', 'if err := error(nil); err != nil {'),
    ("a result past its bound recorded", L, 'err != nil || len(b) > maxResultBytes {', 'err != nil {'),
    # The declassifications, set by Records only.
    ("every ref sanitised", G, 'Sanitised:   producer == types.NodeKindGate && kind == types.ResultKindGateResult,', 'Sanitised:   true,'),
    ("no ref sanitised", G, 'Sanitised:   producer == types.NodeKindGate && kind == types.ResultKindGateResult,', 'Sanitised:   false,'),
    ("a review's decision builder-safe", G, ' && r.Type == types.RefTypeView}', '}'),
    ("another node's view builder-safe", G, 'BuilderSafe: producer == types.NodeKindReview && kind == types.ResultKindReview && r.Type', 'BuilderSafe: r.Type'),
    # Truthful gate lineage, and its ids.
    ("the gate's acceptance edge dropped", Q, 'Lineage: slices.Clone(lineage), Restrictions', 'Lineage: []string{}, Restrictions'),
    ("the gate's lineage not each once, sorted", Q, 'range slices.Compact(slices.Sorted(slices.Values(s.Acceptance)))', 'range s.Acceptance'),
    ("another report's ids read", Q, 'err != nil || r.Schema != reportSchema {', 'err != nil {'),
    ("an id read twice", Q, 'if id != "" && !slices.Contains(out, id) {', 'if id != "" {'),
    ("an overlaid run published with no acceptance", Q, 'if v.overlaid && len(s.Acceptance) == 0 {', 'if false {'),
    ("the overlay not marked", GG, 'v.overlaid = len(in.Overlay) > 0', 'v.overlaid = false'),
    # The walk's declassifications.
    ("an acceptance ancestor only unresolved", I, '\tif w.hidden[id] { // an acceptance blob', '\tif false { // an acceptance blob'),
    ("the builder takes a sanitised report", I, 'w.role == "reviewer" && rec.Sanitised', 'rec.Sanitised'),
    ("the reviewer takes no sanitised report", I, 'w.role == "reviewer" && rec.Sanitised', 'false'),
    ("the reviewer takes a builder-safe view", I, 'w.role == "builder" && rec.BuilderSafe', 'rec.BuilderSafe'),
    ("the builder takes no builder-safe view", I, 'w.role == "builder" && rec.BuilderSafe', 'false'),
    ("a declassified input never expanded as an ancestor", I, 'case w.shallow[p]:', 'case false:'),
    ("records not Load's built from", I, 'case !in.Records.loaded:', 'case false:'),
    ("records not Load's checked against", I, 'case !r.loaded || r.Resolve == nil || r.Acceptance == nil:', 'case r.Resolve == nil || r.Acceptance == nil:'),
    # The fold-side check.
    ("a recorded intent's included refs unchecked", I, '\tfor _, x := range mi.Included {\n\t\trefs = append(refs, x.Ref)\n\t}\n', ''),
    ("a recorded intent's omitted refs unchecked", I, '\tfor _, x := range mi.Omitted {\n\t\trefs = append(refs, x.Ref)\n\t}\n', ''),
    ("another consumer's intent checked as a builder's", I, 'case role != "builder" && role != "reviewer":', 'case false:'),
    ("a recorded intent checked with no acceptance", I, 'case !r.loaded || r.Resolve == nil || r.Acceptance == nil:', 'case !r.loaded || r.Resolve == nil:'),
    ("the fold's check skipped", I, '\treturn Check(r, role, mi)\n', '\treturn nil\n'),
    # The consumer's role, its attempt's node's (Opus #321 H1).
    ("the role taken from the intent", I, '\treturn role, nil\n}', '\treturn mi.Consumer.Role, nil\n}'),
    ("a stated role that differs accepted", I, 'case mi.Consumer.Role != role:', 'case false:'),
    ("another task's intent checked", I, 'case mi.Task == nil || *mi.Task != task:', 'case mi.Task == nil:'),
    ("an intent with no attempt checked", I, '\tcase mi.Attempt == nil:\n\t\treturn "", fmt.Errorf("%w: it names no attempt", ErrConsumer)\n', ''),
    ("an attempt of another task's node accepted", I, 'case !ok || n.Task != task:', 'case !ok:'),
    ("a plan's attempt a builder's", I, 'map[types.NodeKind]string{types.NodeKindBuild: "builder",', 'map[types.NodeKind]string{types.NodeKindPlan: "builder", types.NodeKindBuild: "builder",'),
    # The builder-safe findings view.
    ("no view for a no_go", R, '\t\t\tr.Value = append(r.Value, vref)\n', ''),
    ("a view that is a blob", R, 'Type: types.RefTypeView, ID: vd', 'Type: types.RefTypeBlob, ID: vd'),
    ("a view derived from nothing", R, 'Lineage: []string{decision}', 'Lineage: []string{}'),
    ("a view not bound to its decision", R, '{viewSchema, decision, f.CandidateRound, findings}', '{viewSchema, "", 0, findings}'),
    ("a view with no gate ids supplied", R, 'if f.GateIDs == nil {', 'if false {'),
    ("gate ids kept in titles", R, 'return strings.HasPrefix(low[i:], id)', 'return false'),
    ("a short id's mark first", R, '\tslices.SortFunc(ids, func(a, b string) int { return len(b) - len(a) })', '\tslices.Sort(ids)'),
    ("every id redacted", R, 'len(v) >= minRedacted && ', ''),
    ("a cut id's ellipsis kept", R, '\t\tid = strings.TrimSuffix(id, "…")\n', ''),
    ("an id's segments not redacted", R, '\t\tfor _, seg := range strings.Split(id, "/") {\n\t\t\tadd(seg)\n\t\t}\n', ''),
    ("an id's words not redacted", R, '[]string{id, strings.ReplaceAll(id, "_", " ")}', '[]string{id}'),
    ("ids matched in their case", R, '\t\t\treturn l\n', '\t\t\treturn r\n'),
    ("format characters kept in titles", R, 'case unicode.Is(unicode.Cf, r):', 'case false:'),
    ("control characters kept in titles", R, 'case unicode.IsControl(r):', 'case false:'),
]


def main():
    repo = subprocess.run(['git', 'rev-parse', '--show-toplevel'], capture_output=True, text=True, check=True).stdout.strip()
    jobs = int(sys.argv[1]) if len(sys.argv) > 1 else 4
    sel = [int(a) for a in sys.argv[2:]] or list(range(len(MUTS)))
    work = tempfile.mkdtemp(prefix='review-library-mutations-')
    copies = []
    for w in range(jobs):
        d = os.path.join(work, str(w))
        shutil.copytree(repo, d, symlinks=True, ignore=shutil.ignore_patterns('.git', '.claude'))
        copies.append(d)

    def job(w):
        out = []
        for i in sel[w::jobs]:
            name, f, old, new = MUTS[i]
            p = os.path.join(copies[w], f)
            src = open(p).read()
            if src.count(old) != 1:
                out.append((i, name, 'BADSTRING'))
                continue
            open(p, 'w').write(src.replace(old, new))
            try:
                r = subprocess.run(['go', 'test', '-count=1', '-timeout', '600s', './internal/intent/', './internal/graph/', './internal/gate/', './internal/review/', './internal/fixtures/'],
                                   cwd=copies[w], capture_output=True, text=True)
            finally:
                open(p, 'w').write(src)
            out.append((i, name, 'KILLED' if r.returncode else 'SURVIVED'))
        return out

    with ThreadPoolExecutor(jobs) as ex:
        results = sorted(r for rs in ex.map(job, range(jobs)) for r in rs)
    for i, name, res in results:
        print(i, name, res)
    for d in copies:
        clean = subprocess.run(['diff', '-r', '-q', os.path.join(d, 'internal'), os.path.join(repo, 'internal')], capture_output=True).returncode == 0
        print('copy', os.path.basename(d), 'clean' if clean else 'DIRTY')
    shutil.rmtree(work, ignore_errors=True)
    sys.exit(1 if any(r != 'KILLED' for _, _, r in results) else 0)


if __name__ == '__main__':
    main()
