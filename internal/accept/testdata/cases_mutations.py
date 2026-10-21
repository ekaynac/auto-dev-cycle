# SPDX-License-Identifier: Apache-2.0
#
# The Go test-case identities' mutations (P14d-b1; Astra #292 H3, H4 and
# round 2's 2, 3, 6, 7; Opus #318; Astra #318; Opus #344): each is applied alone to a clean copy of the
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
    ("no runs-to-runs-checks transition", C, '\t\t\thave += max(0, a.ids[c+" runs checks"]-b.ids[c+" runs checks"])\n', '\t\t\t_ = c\n'),
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
    # The direct forms (Astra #318 1).
    ("a literal's t.Run drives its loop (Astra #318 1)", C, '\t\tcase *ast.FuncLit:\n\t\t\tframes = append(frames, frame{})\n', '\t\tcase *ast.FuncLit:\n'),
    ("an inner loop's kind not held by the outer (Opus #344 L1)", C, '\t\t\t\t\tframes[p].k = loopKind{frames[p].k.checks || f.k.checks, frames[p].k.runs || f.k.runs}\n', ''),
    ("a range's own expression drives it", C, ' && x.Pos() >= frames[p].loop.Body.Pos() {', ' {'),
    ("a literal read in place", C, '\t\tcase *ast.FuncLit:\n\t\t\tw.sum.opaque = true\n\t\t\treturn false', '\t\tcase *ast.FuncLit:\n\t\t\treturn true'),
    ("a literal not opaque", C, '\t\tcase *ast.FuncLit:\n\t\t\tw.sum.opaque = true\n', '\t\tcase *ast.FuncLit:\n'),
    ("a Run's literal callback unread", C, '\tif fl, ok := ast.Unparen(x.Args[1]).(*ast.FuncLit); ok {\n\t\ts.walk(w, fl.Body, next)', '\tif fl, ok := ast.Unparen(x.Args[1]).(*ast.FuncLit); ok && false {\n\t\ts.walk(w, fl.Body, next)'),
    ("a skip on another type checks", C, 'slices.Contains(goSkips, sel.Sel.Name) && s.tval(sel.X) != ""', 'slices.Contains(goSkips, sel.Sel.Name)'),
    # Constant conditions.
    ("a dead branch named", C, '\t\tif n == nil || s.over() || s.cs.dead[n] {\n\t\t\treturn false\n\t\t}\n\t\tswitch x := n.(type) {\n\t\tcase *ast.IfStmt:\n\t\t\ts.markDead(x)\n\t\tcase *ast.FuncLit:\n\t\t\tw.sum', '\t\tif n == nil || s.over() {\n\t\t\treturn false\n\t\t}\n\t\tswitch x := n.(type) {\n\t\tcase *ast.IfStmt:\n\t\t\ts.markDead(x)\n\t\tcase *ast.FuncLit:\n\t\t\tw.sum'),
    ("a dead branch printed", C, '\t\tif s.over() || s.cs.dead[n] {\n\t\t\treturn false\n\t\t}\n\t\ttop := stack', '\t\tif s.over() {\n\t\t\treturn false\n\t\t}\n\t\ttop := stack'),
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
    # Build alternatives (Astra #292 H4).
    ("variants merged (H4)", C, '\t\t\tif as == nil { // its constraint changed', '\t\t\tif as = nil; as == nil { // its constraint changed'),
    ("the candidate's variants' most, not least", C, 'out.ids[id] = min(n, c.ids[id])', 'out.ids[id] = max(n, c.ids[id])'),
    # The case layer's own bound (Opus #318 H5, H6, r2 M1; Astra #318 3).
    ("the run's work, not the layer's (H6)", C, '\ts.r.caseWork += n\n', '\ts.r.work += n\n\ts.r.caseWork = s.r.work\n'),
    ("the run's cancellation unread", C, ' || s.r.ctx.Err() != nil }', ' }'),
    ("a test body uncharged", C, '\tif !s.caseCharge(int(body.End() - body.Pos())) {\n\t\treturn w.sum\n', '\tif false {\n\t\treturn w.sum\n'),
    ("the kinds' pass uncharged", C, '\tif !s.caseCharge(int(body.End() - body.Pos())) {\n\t\treturn out\n', '\tif false {\n\t\treturn out\n'),
    ("canon's span uncharged (Opus #344 H1)", C, '\t\tif !s.caseCharge(len(span)) {\n\t\t\treturn\n\t\t}\n\t\tt.scan(span)', '\t\tt.scan(span)'),
    ("a number's prefix kept (Opus #344 r2 M1)", C, "\t\tcase i == 1 && b[0] == '0' && (c == 'X' || c == 'B' || c == 'O'):", "\t\tcase false:"),
    ("a number's exponent kept", C, "\t\tcase hex && c == 'P', !hex && c == 'E':", "\t\tcase hex && false, !hex && false:"),
    ("a hex digit lower-cased", C, "\t\tcase hex && c == 'P', !hex && c == 'E':", "\t\tcase (hex || !hex) && (c == 'P' || c == 'E'):"),
    ("a comma before a closer kept", C, '\tif t.comma && !closing {', '\tif t.comma && (closing || !closing) {'),
    ("an event's literal bodies read whole (Opus #344 r2 L1)", C, '\t\t\te := sha256.Sum256(append(sum[:], s.eventText(n)...))', '\t\t\te := sha256.Sum256(append(sum[:], s.canon(n)...))'),
    ("exits compared only with equal counts (Opus #344 r2 L2)", C, ' || bs.exits != as.exits || rowsChanged', ' || rowsChanged'),
    ("canon keeps semicolons", C, '\t\tcase token.SEMICOLON: // a newline\'s or a written one: gofmt moves between them\n\t\t\tcontinue', '\t\tcase token.SEMICOLON: // a newline\'s or a written one: gofmt moves between them\n\t\t\tt.write(tok, ";")\n\t\t\tcontinue'),
    ("canon without separators", C, "\tif !t.first {\n\t\tt.b.WriteByte(' ')\n\t}", "\tif !t.first {\n\t}"),
    ("no return or branch in the print (Opus #344 M1)", C, '\t\tcase *ast.ReturnStmt, *ast.BranchStmt: // an early exit', '\t\tcase *ast.EmptyStmt: // an early exit'),
    ("a row's identity uncharged, read on", C, '\t\tif !s.caseCharge(len(k)) {\n\t\t\treturn\n\t\t}\n\t\tw.sum.ids[k]++\n', '\t\tw.sum.ids[k]++\n'),
    ("a long prefix kept whole (H5)", C, '\tif len(next) > maxPrefix {', '\tif false {'),
    ("past the bound, a test still verified", C, 'if fa.cases[k][cons] = c; s.over() && fa.bad[path] == "" {', 'if fa.cases[k][cons] = c; false {'),
]


def main():
    repo = subprocess.run(['git', 'rev-parse', '--show-toplevel'], capture_output=True, text=True, check=True).stdout.strip()
    jobs = int(sys.argv[1]) if len(sys.argv) > 1 else 4
    sel = [int(a) for a in sys.argv[2:]] or list(range(len(MUTS)))
    if os.path.realpath(tempfile.gettempdir()).startswith(os.path.realpath(os.path.expanduser('~'))):
        sys.exit('the copies must not be under $HOME (testguard fails every run there): unset TMPDIR')
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
                vet = subprocess.run(['go', 'vet', './internal/accept/'], cwd=copies[w], capture_output=True, text=True)
                if vet.returncode:  # a mutation that does not compile proves nothing
                    out.append((i, name, 'NOCOMPILE'))
                    continue
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
    counts = {k: sum(r == k for _, _, r in results) for k in ('KILLED', 'SURVIVED', 'NOCOMPILE', 'BADSTRING')}
    print(' '.join(f'{k.lower()} {n}' for k, n in counts.items()))
    sys.exit(1 if any(r != 'KILLED' for _, _, r in results) else 0)


if __name__ == '__main__':
    main()
