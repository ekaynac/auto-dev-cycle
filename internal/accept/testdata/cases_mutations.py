# SPDX-License-Identifier: Apache-2.0
#
# The Go test-case identities' mutations (P14d-b; Astra #292 H3, H4 and
# round 2's 2, 3, 5 to 8; Opus #318): each is applied alone to a clean copy of the
# repository, and `go test -run TestIntegrityCases ./internal/accept/` must
# fail on it (KILLED). Run from the repository root:
#
#   python3 internal/accept/testdata/cases_mutations.py [jobs] [index ...]
#
# The copies keep symlinks (the adapters' node_modules).
import os
import shutil
import subprocess
import sys
import tempfile
from concurrent.futures import ThreadPoolExecutor

C = 'internal/accept/integrity_cases.go'
MUTS = [
    # Identities and their comparison.
    ("a removed case not counted", C, 'if d := b.ids[k] - have; d > 0 {', 'if d := b.ids[k] - have; d > 99 {'),
    ("no runs-to-runs-checks transition", C, '\t\t\thave += max(0, a.ids[c+" runs checks"]-b.ids[c+" runs checks"])\n', ''),
    ("runs to checks accepted (Opus #318 H1)", C, '\t\t\thave += max(0, a.ids[c+" runs checks"]-b.ids[c+" runs checks"])\n', '\t\t\thave += max(0, a.ids[c+" checks"]-b.ids[c+" checks"]) + max(0, a.ids[c+" runs checks"]-b.ids[c+" runs checks"])\n'),
    ("the transition for subtests too (r2 2)", C, 'ok && strings.HasPrefix(k, "row ") {', 'ok {'),
    ("a loop's runs bit unlabelled (H1)", C, '\t\tkind = append(kind, "runs")\n', ''),
    ("a loop's checks bit unlabelled", C, '\t\tkind = append(kind, "checks")\n', ''),
    ("opaque never named", C, 'case n > 0 && as.opaque:', 'case false:'),
    ("a computed name followed", C, '} else if !mentionsAny(x.Args[0], s.info, w.traced) {', '} else if false {'),
    ("a traced name read as opaque", C, '} else if !mentionsAny(x.Args[0], s.info, w.traced) {', '} else if true {'),
    ("a table used elsewhere followed", C, '\t\t\tdelete(lits, o)\n', ''),
    ("a nameless row named by its text (M1)", C, 'name = "(nameless)"', 'name = s.canon(key)'),
    ("rows' text unchecked", C, ' || rowsChanged(bs, as)', ''),
    ("the print unchecked", C, 'bs.print != as.print && bt.asserts', 'false && bt.asserts'),
    # Constant conditions.
    ("a dead branch named", C, '\t\tif n == nil || s.over() || s.cs.dead[n] {', '\t\tif n == nil || s.over() {'),
    ("a dead branch printed", C, '\t\tif s.over() || s.cs.dead[n] {', '\t\tif s.over() {'),
    ("a dead else unmarked", C, '} else if s.constCond(x.Cond, true) && x.Else != nil {', '} else if false {'),
    # The print's controls (r2 3, 7; Opus #318 L2).
    ("a case clause's values unprinted", C, '\t\t\tfor _, e := range x.List {', '\t\t\tfor _, e := range x.List[:0] {'),
    ("an else if's own condition dropped (r2 7)", C, '\t\t\ts.control(&ctl, "if", x.Init, x.Cond)', '\t\t\tif ctl.Len() == 0 {\n\t\t\t\ts.control(&ctl, "if", x.Init, x.Cond)\n\t\t\t}'),
    ("no else marker", C, '\t\t\tctl.WriteString("else ")\n', ''),
    ("a case clause past the bound read on", C, '\t\t\t\tif s.control(&ctl, "", e); s.over() {', '\t\t\t\tif s.control(&ctl, "", e); false {'),
    ("a for's parts unprinted", C, 's.control(&ctl, "for", x.Init, x.Cond, x.Post)', 's.control(&ctl, "for")'),
    ("a switch's tag unprinted", C, 's.control(&ctl, "switch", x.Init, x.Tag)', 's.control(&ctl, "switch", x.Init)'),
    ("a type switch's guard unprinted", C, 's.control(&ctl, "type switch", x.Init, x.Assign)', 's.control(&ctl, "type switch", x.Init)'),
    ("a comm clause unprinted", C, 's.control(&ctl, "comm", x.Comm)', 's.control(&ctl, "comm")'),
    # Literals and closures (r2 8; Opus #318 H2, M2).
    ("a closure's Cleanup not followed (r2 8)", C, 'case ok && sel.Sel.Name == "Cleanup" &&', 'case false && sel.Sel.Name == "Cleanup" &&'),
    ("every literal read in place (H2)", C, '\t\t\treturn inPlace[x]\n', '\t\t\treturn true\n'),
    ("a literal called on the spot unread", C, '\t\t\tif fl, ok := ast.Unparen(x.Fun).(*ast.FuncLit); ok {\n\t\t\t\tinPlace[fl] = true\n\t\t\t}\n', ''),
    ("a helper's literal argument read in place (r2 H1)", C, '\t\t\t\ts.compose(w, s.helperSum(d), prefix)\n\t\t\t}\n\t\t}\n\t\treturn true', '\t\t\t\ts.compose(w, s.helperSum(d), prefix)\n\t\t\t\tfor _, a := range x.Args {\n\t\t\t\t\tif fl, ok := ast.Unparen(a).(*ast.FuncLit); ok {\n\t\t\t\t\t\tinPlace[fl] = true\n\t\t\t\t\t}\n\t\t\t\t}\n\t\t\t}\n\t\t}\n\t\treturn true'),
    ("an unread closure not opaque (H2 b)", C, '(!w.defs[x] || w.unread[x])', '!w.defs[x]'),
    ("a valued closure unfollowed (M2)", C, '\t\tif changed[o] {\n\t\t\tdelete(closures, o)\n\t\t}\n', '\t\tif changed[o] || valued[o] {\n\t\t\tdelete(closures, o)\n\t\t}\n'),
    ("closure summaries never kept", C, '\ts.keep(fl, sub.sum)\n', ''),
    # Helpers (Astra #292 H3; Opus #318 H3, H4).
    ("a helper not followed (H3)", C, '\t\t\t} else if d := s.helper(x); d != nil {\n\t\t\t\ts.compose(w, s.helperSum(d), prefix)', '\t\t\t} else if d := s.helper(x); d != nil && false {\n\t\t\t\ts.compose(w, s.helperSum(d), prefix)'),
    ("a named callback not followed (H3)", C, '} else if d := s.declOf(cb); d != nil {', '} else if d := s.declOf(cb); d != nil && false {'),
    ("a summary without its caller's prefix", C, '\t\tk := id[:4] + prefix + id[4:]\n', '\t\tk := id\n'),
    ("a helper's unnamed subtest drives nothing (Opus #318 H3)", C, 'if h := s.helperSum(d); h.runs || len(h.ids) > 0 {', 'if h := s.helperSum(d); len(h.ids) > 0 {'),
    ("a t.Run not marked as running", C, '\tw.sum.runs = true\n', ''),
    ("a helper of several declarations followed (H4)", C, '\tif len(ds) > 1 {\n', '\tif false {\n'),
    ("a declaration's text undigested (H4)", C, '\t\t\t_, _ = io.WriteString(sum, s.canon(&text)+"\\x00")\n', ''),
    ("a cycle's summary kept", C, '\tif !h.cyc && !s.over() {', '\tif !s.over() {'),
    # Build alternatives (Astra #292 H4).
    ("variants merged (H4)", C, '\t\t\tif as == nil { // its constraint changed', '\t\t\tif as = nil; as == nil { // its constraint changed'),
    ("the candidate's variants' most, not least", C, 'out.ids[id] = min(n, c.ids[id])', 'out.ids[id] = max(n, c.ids[id])'),
    # drives (r2 5, 6; Opus #318 M3).
    ("drives' cache without depth (r2 5)", C, 'key := driveKey{body, runs, depth}', 'key := driveKey{body, runs, 0}'),
    ("drives reads past the bound (r2 6)", C, '\tif !s.caseCharge(int(body.End() - body.Pos())) {\n\t\treturn driveNo\n', '\tif s.caseCharge(int(body.End() - body.Pos())); false {\n\t\treturn driveNo\n'),
    ("drives' depth unbounded", C, '\tif depth > maxCaseDepth {\n\t\treturn driveCut\n', '\tif false {\n\t\treturn driveCut\n'),
    ("a cut loop not driving (M3)", C, '\tif checks == driveNo && runs == driveNo {', '\tif checks != driveYes && runs != driveYes {'),
    ("a closure's reading uncharged", C, '\tif !s.caseCharge(int(fl.Body.End() - fl.Body.Pos())) { // each reading paid for', '\tif false {'),
    # The case layer's own bound (Opus #318 H5, H6).
    ("the run's work, not the layer's (H6)", C, '\ts.r.caseWork += n\n', '\ts.r.work += n\n\ts.r.caseWork = s.r.work\n'),
    ("a per-scan counter, not the run's (r2 M1)", C, '\ts.cs = caseScan{map[driveKey]drive{}', '\ts.r.caseWork = 0\n\t\ts.cs = caseScan{map[driveKey]drive{}'),
    ("the run's cancellation unread", C, ' || s.r.ctx.Err() != nil }', ' }'),
    ("a body uncharged", C, '\tif !s.caseCharge(int(body.End() - body.Pos())) {\n\t\treturn w.sum\n', '\tif false {\n\t\treturn w.sum\n'),
    ("a row's identity uncharged, read on", C, '\t\tif !s.caseCharge(len(k)) {\n\t\t\treturn\n\t\t}\n\t\tw.sum.ids[k]++\n', '\t\tw.sum.ids[k]++\n'),
    ("a composed identity uncharged", C, '\t\tif !s.caseCharge(len(k)) {\n\t\t\treturn\n\t\t}\n\t\tw.sum.ids[k] += n\n', '\t\tw.sum.ids[k] += n\n'),
    ("a long prefix kept whole (H5)", C, '\tif len(next) > maxPrefix {', '\tif false {'),
    ("past the bound, a test still verified", C, 'if fa.cases[k][cons] = c; s.over() && fa.bad[path] == "" {', 'if fa.cases[k][cons] = c; false {'),
]


def main():
    repo = subprocess.run(['git', 'rev-parse', '--show-toplevel'], capture_output=True, text=True, check=True).stdout.strip()
    jobs = int(sys.argv[1]) if len(sys.argv) > 1 else 4
    sel = [int(a) for a in sys.argv[2:]] or list(range(len(MUTS)))
    work = tempfile.mkdtemp(prefix='cases-mutations-')
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
                r = subprocess.run(['go', 'test', '-count=1', '-timeout', '600s', '-run', 'TestIntegrityCases', './internal/accept/'], cwd=copies[w], capture_output=True, text=True)
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
