# SPDX-License-Identifier: Apache-2.0
#
# 22a-6L's mutations (W17's library-level parts: #226 entries 91, 94(2);
# the Opus #321 answers: H1, H2, M1, M2, L1-L5; the Astra #321 answers:
# H1, M3-M5, L6; the Opus #321 re-check answers: M1, M2, L1-L4):
# each is applied alone to a clean copy of the repository. A mutation must
# compile (`go vet` on the mutated copy passes; NOCOMPILE otherwise: a
# kill by a compile error proves nothing about the tests, Opus #321 r2
# L1), and then the tests of intent, graph, gate, review and fixtures must
# fail on it (KILLED). Run from the repository root, with TMPDIR outside
# $HOME (the test guard fails every test under a home TMPDIR, so every
# mutation would read KILLED):
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
L, GG, W = 'internal/graph/lifecycle.go', 'internal/gate/gate.go', 'internal/eventstore/writer.go'
PKGS = ['./internal/intent/', './internal/graph/', './internal/gate/', './internal/review/', './internal/fixtures/']
MUTS = [
    # The bounded decode, the task's own (Opus #321 M1, L5).
    ("a result past its bound decoded", G, 'size > int64(maxResultBytes) || read > int64(maxResultsBytes)', 'read > int64(maxResultsBytes)'),
    ("the results past their bound decoded", G, 'size > int64(maxResultBytes) || read > int64(maxResultsBytes)', 'size > int64(maxResultBytes)'),
    ("a node record read unbounded", G, '!ok || size > int64(maxNodeBytes)', '!ok || size < 0'),
    ("the task's result index not written", L, 'if err := put(tx, TaskResultProjection, string(rec.Recorded.Task)+"/"+string(p.Result.ID), true); err != nil {', 'if err := error(nil); err != nil {'),
    ("a result past its bound recorded", L, 'err != nil || len(b) > maxResultBytes {', 'err != nil || len(b) < 0 {'),
    # The task's index range, bounded (Astra #321 M5; Opus #321 r2 M1).
    ("the results listed past their bound", G, 'case len(keys) > maxTaskResults:', 'case false:'),
    ("the keys read unlimited", W, 'return t.keys(projection, prefix, n)', 'return t.keys(projection, prefix, -1)'),
    ("a negative limit taken", W, 'err != nil || n < 0 {', 'err != nil {'),
    ("an empty prefix taken", W, 'if err := checkProjection(projection, prefix); err != nil || n < 0 {', 'if err := error(nil); err != nil || n < 0 {'),
    # The declassification candidates, set by Records only (Astra #321 H1).
    ("every ref a sanitised candidate", G, 'Sanitised: sanitised, BuilderSafe: safe}', 'Sanitised: true, BuilderSafe: safe}'),
    ("no ref a sanitised candidate", G, 'Sanitised: sanitised, BuilderSafe: safe}', 'Sanitised: false, BuilderSafe: safe}'),
    ("a gate result's shape ignored", G, 'gate = gate && kind == types.NodeKindGate', 'gate = kind == types.NodeKindGate'),
    ("a report alone a candidate", G, 'receipts == 1 && blobs == 1', 'blobs == 1'),
    ("a review's decision builder-safe", G, ' && r.Type == types.RefTypeView\n', '\n'),
    ("another node's view builder-safe", G, 'safe := kind == types.NodeKindReview && rr.Result.Kind == types.ResultKindReview && r.Type', 'safe := r.Type'),
    # recordResult refuses a gate result's other attachments (Astra #321 H1; Opus #321 r2 L2).
    ("a gate result's attachment recorded", L, 'r.Result.Kind == types.ResultKindGateResult && !ok {', 'r.Result.Kind == types.ResultKindGateResult && !ok && false {'),
    ("a gate result of any refs allowed", G, 'return receipts <= 1 && blobs <= 1,', 'return true,'),
    # The verified declassifications (Astra #321 H1).
    ("a candidate not verified", I, '\t\tif safe {\n', '\t\tif safe || true {\n'),
    ("a gate report's bytes unchecked", I, 'if err != nil || !ok || gate.CheckReport(b) != nil {', 'if err != nil || !ok {'),
    ("a findings view's bytes unchecked", I, 'return review.CheckView(view, decision, dec.ID, gateIDs) == nil, nil', 'return review.CheckView(view, decision, dec.ID, gateIDs) == nil || true, nil'),
    ("a blob's digest unchecked", I, 'len(b) <= maxSafeBytes && bytes.Equal(', 'len(b) <= maxSafeBytes || bytes.Equal('),
    ("a blob past its bound read", I, 'return b, len(b) <= maxSafeBytes && ', 'return b, '),
    ("a report's bytes not Report's exactly", Q, 'if again, err := json.Marshal(r); err != nil || !bytes.Equal(again, b) {', 'if again, err := json.Marshal(r); err != nil || again == nil {'),
    ("a view's bytes not ViewBytes' exactly", R, 'if err == nil && !bytes.Equal(want, view) {', 'if err == nil && !bytes.Equal(want, view) && false {'),
    ("a go decision's view accepted", R, 'case d.Decision != NoGo:', 'case false:'),
    # The content of a declassified gate output (Opus #321 r2 M2).
    ("a report's outcome unchecked", Q, 'case !slices.Contains([]vocab.Outcome{', 'case false && !slices.Contains([]vocab.Outcome{'),
    ("a report's reason unchecked", Q, 'case rerr != nil:', 'case rerr != nil && false:'),
    ("a report's tree unchecked", Q, 'case !hexID(r.Tree):', 'case false:'),
    ("a tree of any length", Q, 'if len(s) != 40 && len(s) != 64 {', 'if len(s) == 0 {'),
    ("an upper-case tree", Q, "if (c < '0' || c > '9') && (c < 'a' || c > 'f') {", "if (c < '0' || c > '9') && (c < 'A' || c > 'f') {"),
    ("a report's detail unbounded", Q, 'case len(r.Detail) > MaxDetail:', 'case false:'),
    ("a negative elapsed time", Q, 'r.ElapsedMS < 0 || r.Tests < 0', 'r.Tests < 0'),
    ("negative tests", Q, 'r.ElapsedMS < 0 || r.Tests < 0 || r.Truncated < 0', 'r.ElapsedMS < 0 || r.Truncated < 0'),
    ("negative truncated", Q, 'r.Tests < 0 || r.Truncated < 0:', 'r.Tests < 0:'),
    ("a finding's outcome unchecked", Q, 'if !slices.Contains([]results.Outcome{', 'if false && !slices.Contains([]results.Outcome{'),
    ("a sanitised receipt unread", I, 'return gateReceipt(b, rec.ID), nil', 'return gateReceipt(b, rec.ID) || true, nil'),
    ("a receipt of any kind", I, 'r.Kind != types.ReceiptKindGate || ', ''),
    ("a receipt under another id", I, 'string(r.ID) != id || ', ''),
    ("a receipt's detail unbounded", I, 'string(r.ID) != id || len(r.Detail) > gate.MaxDetail {', 'string(r.ID) != id {'),
    ("a receipt predicate's detail unbounded", I, 'if len(p.Detail) > gate.MaxDetail {', 'if len(p.Detail) > gate.MaxDetail && false {'),
    # Truthful gate lineage, and its ids.
    ("the gate's acceptance edge dropped", Q, 'Lineage: slices.Clone(lineage), Restrictions', 'Lineage: []string{}, Restrictions'),
    ("the gate's lineage not each once, sorted", Q, 'range slices.Compact(slices.Sorted(slices.Values(s.Acceptance)))', 'range s.Acceptance'),
    ("another report's ids read", Q, 'err != nil || r.Schema != reportSchema {\n\t\treturn nil, fmt.Errorf', 'err != nil {\n\t\treturn nil, fmt.Errorf'),
    ("an id read twice", Q, 'if id != "" && !slices.Contains(out, id) {', 'if id != "" {'),
    ("a clean report's ids nil (Astra #321 M3)", Q, '\tout := []string{}\n', '\tvar out []string\n'),
    ("an overlaid run published with no acceptance", Q, 'if v.overlaid && len(s.Acceptance) == 0 {', 'if false {'),
    ("the overlay not marked", GG, 'v.overlaid = len(in.Overlay) > 0', 'v.overlaid = false'),
    # The walk's declassifications.
    ("an acceptance ancestor only unresolved", I, '\tif w.hidden[id] { // an acceptance blob', '\tif false { // an acceptance blob'),
    ("the builder takes a sanitised report", I, 'w.role == "reviewer" && rec.Sanitised', 'rec.Sanitised'),
    ("the reviewer takes no sanitised report", I, 'w.role == "reviewer" && rec.Sanitised', 'false'),
    ("the reviewer takes a builder-safe view", I, 'w.role == "builder" && rec.BuilderSafe', 'rec.BuilderSafe'),
    ("the builder takes no builder-safe view", I, 'w.role == "builder" && rec.BuilderSafe', 'false'),
    ("a declassified input never expanded as an ancestor", I, 'case w.shallow[p]:', 'case false:'),
    ("a view's unresolved decision input skipped (Opus #321 r2 L3)", I, '\t\t\tif !ok {\n\t\t\t\treturn false, nil\n\t\t\t}\n\t\t\tif !pr.Sanitised', '\t\t\tif !ok {\n\t\t\t\tcontinue\n\t\t\t}\n\t\t\tif !pr.Sanitised'),
    # Records: Load's only, immutable (Opus #321 L4, Astra #321 M4).
    ("records with no resolver used", I, 'return r.resolve != nil && r.acceptance != nil && r.safe != nil', 'return r.acceptance != nil && r.safe != nil'),
    ("records with no acceptance used", I, 'return r.resolve != nil && r.acceptance != nil && r.safe != nil', 'return r.resolve != nil && r.safe != nil'),
    ("records with no verifier used", I, 'return r.resolve != nil && r.acceptance != nil && r.safe != nil', 'return r.resolve != nil && r.acceptance != nil'),
    ("a replaceable resolver exported", I, '\tsafe       func(graph.RefRecord) (bool, error)\n}', '\tsafe       func(graph.RefRecord) (bool, error)\n\tResolver   graph.RefRecords\n}'),
    ("the acceptance handed out", I, 'return slices.Clone(r.acceptance) }', 'return r.acceptance }'),
    # The fold-side check.
    ("a recorded intent's included refs unchecked", I, '\tfor _, x := range mi.Included {\n\t\trefs = append(refs, x.Ref)\n\t}\n', ''),
    ("a recorded intent's omitted refs unchecked", I, '\tfor _, x := range mi.Omitted {\n\t\trefs = append(refs, x.Ref)\n\t}\n', ''),
    ("another consumer's intent checked as a builder's", I, 'case role != "builder" && role != "reviewer":', 'case false:'),
    ("a recorded intent checked against records not Load's", I, '\tcase !r.loaded():\n\t\treturn fmt.Errorf("%w: no records", ErrAncestryUnresolved)\n', ''),
    ("the fold's check skipped", I, '\treturn Check(r, role, mi)\n', '\t_ = Check(r, role, mi)\n\treturn nil\n'),
    # The consumer's role, its attempt's node's (Opus #321 H1).
    ("the role taken from the intent", I, '\treturn role, nil\n}', '\t_ = role\n\treturn mi.Consumer.Role, nil\n}'),
    ("a stated role that differs accepted", I, 'case mi.Consumer.Role != role:', 'case false:'),
    ("another task's intent checked", I, 'case mi.Task == nil || *mi.Task != task:', 'case mi.Task == nil:'),
    ("an intent with no attempt checked", I, '\tcase mi.Attempt == nil:\n\t\treturn "", fmt.Errorf("%w: it names no attempt", ErrConsumer)\n', ''),
    ("an attempt of another task's node accepted", I, 'case !ok || n.Task != task:', 'case !ok:'),
    ("a plan's attempt a builder's", I, 'map[types.NodeKind]string{types.NodeKindBuild: "builder",', 'map[types.NodeKind]string{types.NodeKindPlan: "builder", types.NodeKindBuild: "builder",'),
    # The builder-safe findings view.
    ("no view for a no_go", R, '\t\t\tr.Value = append(r.Value, vref)\n', '\t\t\t_ = vref\n'),
    ("a view that is a blob", R, 'Type: types.RefTypeView, ID: vd', 'Type: types.RefTypeBlob, ID: vd'),
    ("a view derived from nothing", R, 'Lineage: []string{decision}', 'Lineage: []string{}'),
    ("a view not bound to its decision", R, 'viewRecord{viewSchema, decision, round, findings}', 'viewRecord{viewSchema, "", 0, findings}'),
    ("a view with no gate ids supplied", R, 'if gateIDs == nil {', 'if false {'),
    ("gate ids kept in titles", R, 'return strings.HasPrefix(n[i:], id)', 'return false'),
    ("a short id's mark first", R, '\t\tslices.SortFunc(ids, func(a, b string) int { return len(b) - len(a) })', '\t\tslices.Sort(ids)'),
    ("every id redacted", R, 'len(v) >= minRedacted && ', ''),
    ("a cut id's ellipsis kept", R, '\t\tid = strings.TrimSuffix(id, "…")\n', ''),
    ("an id's segments not redacted", R, '\t\tfor _, seg := range strings.Split(id, "/") {\n\t\t\tadd(seg)\n\t\t}\n', ''),
    ("control characters kept in titles", R, 'case c < \' \' || c == 0x7f:', 'case false:'),
    # Ids and titles normalized alike, ASCII only, fail closed (Astra #321 L6).
    ("ids matched in their case", R, "c += 'a' - 'A'", 'c += 0'),
    ("white space runs not one space", R, '\t\t\tif !space {\n', '\t\t\tif !space || true {\n'),
    ("separators not spaces", R, " || c == '_' || c == '-':", ':'),
    ("a title not ASCII matched in part", R, 'case c >= 0x80:\n\t\t\treturn "", nil, false', 'case false:\n\t\t\treturn "", nil, false'),
    ("an id not ASCII ignored", R, 'm.all = m.all && ok', 'm.all = m.all && (ok || true)'),
    ("a match's bytes not all redacted", R, 'for o := first; o <= last; o++ {', 'for o := first; o < last; o++ {'),
    # The compact form, an id's separators removed (Opus #321 r2 L4).
    ("titles not matched compact", R, 'n, at, ok := normalize(title, f == 1)', 'n, at, ok := normalize(title, f < 0)'),
    ("ids not marked compact", R, 'v, _, ok := normalize(id, f == 1)', 'v, _, ok := normalize(id, false)'),
    ("the compact form keeps separators", R, "case compact && (c < 'a' || c > 'z') && (c < '0' || c > '9'):", "case compact && false:"),
]


def main():
    home = os.path.realpath(os.path.expanduser('~'))
    tmp = os.path.realpath(tempfile.gettempdir())
    if tmp == home or tmp.startswith(home + os.sep):
        sys.exit(f'mutations: TMPDIR {tmp} is under $HOME: the test guard fails every test there, so every mutation would read KILLED; set TMPDIR outside it')
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
                vet = subprocess.run(['go', 'vet'] + PKGS, cwd=copies[w], capture_output=True, text=True)
                if vet.returncode:
                    out.append((i, name, 'NOCOMPILE'))
                    continue
                r = subprocess.run(['go', 'test', '-count=1', '-timeout', '600s'] + PKGS, cwd=copies[w], capture_output=True, text=True)
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
