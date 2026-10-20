# SPDX-License-Identifier: Apache-2.0
#
# The Go test-case identities' mutations (P14d-b; Astra #292 H3, H4 and
# round 2's 2, 3, 5 to 8): each is applied alone to a clean copy of the
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
    ("no runs-to-checks transition", C, '\t\t\thave += max(0, a.ids[c+" checks"]-b.ids[c+" checks"])\n', ''),
    ("the transition for subtests too (r2 2)", C, 'ok && strings.HasPrefix(k, "row ") {', 'ok {'),
    ("rows whatever the loop's kind", C, 'map[bool]string{true: " checks", false: " runs"}', 'map[bool]string{true: "", false: ""}'),
    ("opaque never named", C, 'case n > 0 && as.opaque:', 'case false:'),
    ("a computed name followed", C, '} else if !mentionsAny(x.Args[0], s.info, w.traced) {', '} else if false {'),
    ("a table used elsewhere followed", C, '\t\t\tdelete(lits, o)\n', ''),
    ("rows' text unchecked", C, ' || rowsChanged(bs, as)', ''),
    ("the print unchecked", C, 'bs.print != as.print && bt.asserts', 'false && bt.asserts'),
    # Constant conditions.
    ("a dead branch named", C, '\t\tif n == nil || s.r.work > maxWork || s.cs.dead[n] {', '\t\tif n == nil || s.r.work > maxWork {'),
    ("a dead branch printed", C, '\t\tif s.r.work > maxWork || s.cs.dead[n] {', '\t\tif s.r.work > maxWork {'),
    # The print's controls (r2 3, 7).
    ("a case clause's values unprinted", C, '\t\t\tfor _, e := range x.List {', '\t\t\tfor _, e := range x.List[:0] {'),
    ("an else if's own condition dropped (r2 7)", C, '\t\t\ts.control(&ctl, "if", x.Init, x.Cond)', '\t\t\tif ctl.Len() == 0 {\n\t\t\t\ts.control(&ctl, "if", x.Init, x.Cond)\n\t\t\t}'),
    ("no else marker", C, '\t\t\tctl.WriteString("else ")\n', ''),
    ("a case clause past the bound read on", C, '\t\t\t\tif s.control(&ctl, "", e); s.r.work > maxWork {', '\t\t\t\tif s.control(&ctl, "", e); false {'),
    # Closures and helpers (H3, r2 8).
    ("a closure's Cleanup not followed (r2 8)", C, 'case ok && sel.Sel.Name == "Cleanup" &&', 'case false && sel.Sel.Name == "Cleanup" &&'),
    ("a helper not followed (H3)", C, '\t\t\t} else if d := s.helper(x); d != nil {\n\t\t\t\ts.compose(w, s.helperSum(d), prefix)', '\t\t\t} else if d := s.helper(x); d != nil && false {\n\t\t\t\ts.compose(w, s.helperSum(d), prefix)'),
    ("a named callback not followed (H3)", C, '} else if d := s.declOf(cb); d != nil {', '} else if d := s.declOf(cb); d != nil && false {'),
    ("a summary without its caller's prefix", C, '\t\tw.sum.ids[id[:4]+prefix+id[4:]] += n', '\t\tw.sum.ids[id] += n'),
    ("a followed closure read where defined too", C, '\t\t\treturn !w.defs[x] // a followed closure is read where it runs', '\t\t\treturn true'),
    ("closure summaries never kept", C, '\ts.keep(fl, sub.sum)\n', ''),
    # Build alternatives (H4).
    ("variants merged (H4)", C, '\t\t\tif as == nil { // its constraint changed', '\t\t\tif as = nil; as == nil { // its constraint changed'),
    # drives (r2 5, 6) and the bound.
    ("drives' cache without depth (r2 5)", C, 'key := driveKey{body, runs, depth}', 'key := driveKey{body, runs, 0}'),
    ("drives reads past the bound (r2 6)", C, 'if !s.charge(int(body.End()-body.Pos())) || depth > maxCaseDepth {', 'if s.charge(int(body.End()-body.Pos())); depth > maxCaseDepth {'),
    ("a summary too deep composed (r2 5)", C, '\tif h.height >= maxCaseDepth {\n', '\tif false {\n'),
    ("summaries' heights not counted", C, '\tw.sum.height = max(w.sum.height, h.height+1)\n', ''),
    ("past the bound, a test still verified", C, 'if fa.cases[k][cons] = c; s.r.work > maxWork {', 'if fa.cases[k][cons] = c; false {'),
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
