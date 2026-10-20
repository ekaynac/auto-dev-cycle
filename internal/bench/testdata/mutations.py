# SPDX-License-Identifier: Apache-2.0
#
# The benchmark recorder's mutations (P25): each is applied alone to a
# clean copy of the repository, and `go test ./internal/bench/` must fail on
# it (KILLED). Run from the repository root:
#
#   python3 internal/bench/testdata/mutations.py [jobs] [index ...]
import os
import shutil
import subprocess
import sys
import tempfile
from concurrent.futures import ThreadPoolExecutor

B, C = 'internal/bench/bench.go', 'internal/bench/compare.go'
MUTS = [
    ("an unattributed measurement dropped", B, '\t\tif !ok {\n\t\t\treturn bt, fmt.Errorf("measurement %s of attempt %s, which is no node\'s", u.ID, u.Attempt)', '\t\tif !ok {\n\t\t\tcontinue'),
    ("an attempt of two nodes accepted", B, 'if _, dup := stageOf[a]; dup {', 'if false {'),
    ("an unknown stage accepted", B, 'if !slices.Contains(Stages, n.Stage) {', 'if false {'),
    ("done before claimed accepted", B, '} else if !n.Done.IsZero() && n.Done.Before(n.Claimed) {', '} else if false {'),
    ("closed before approval accepted", B, 'case !t.Closed.IsZero() && t.Closed.Before(t.Approved):', 'case false:'),
    ("an uncertain call settled", B, '\t\tif c.Uncertain {\n\t\t\tinto = outstanding', '\t\tif false {\n\t\t\tinto = outstanding'),
    ("an unfinished node counted", B, '\t\tif n.Done.IsZero() {\n\t\t\tst.Unfinished++', '\t\tif false {\n\t\t\tst.Unfinished++'),
    ("rounds as nodes", B, 'used, rounds[n.Round] = true, true', 'used, rounds[len(rounds)] = true, true'),
    ("cache tokens dropped", B, 'st.Tokens.CacheRead, st.Tokens.CacheWrite = st.Tokens.CacheRead+c.Usage.CacheRead, st.Tokens.CacheWrite+c.Usage.CacheWrite', 'st.Tokens.CacheRead, st.Tokens.CacheWrite = 0, 0'),
    ("rescues as interventions", B, 'if iv.Stage != s || iv.Kind == Rescue {', 'if iv.Stage != s {'),
    ("rescues not counted", B, 'bt.Rescues += b2i(iv.Kind == Rescue)', 'bt.Rescues += 0'),
    ("a hold dropped", B, '\tcase t.Hold != "":\n\t\th := t.Hold', '\tcase false:\n\t\th := t.Hold'),
    ("an unknown intervention accepted", B, 'return st, false, fmt.Errorf("an unknown intervention %q", iv.Kind)', 'return st, used, nil'),
    ("a changed outcome unreported", C, 'if a, b := verdict(p), verdict(t); a != b {', 'if false {'),
    ("a lower metric reported", C, '\t\t\t\tif m.is > m.was {', '\t\t\t\tif m.is != m.was {'),
    ("a task gone unreported", C, '\t\tif !seen[t.Task] {', '\t\tif false {'),
    ("wall time growth unreported", C, 'if p.WallMs != nil && t.WallMs != nil && *t.WallMs > *p.WallMs {', 'if false {'),
]



def main():
    repo = subprocess.run(['git', 'rev-parse', '--show-toplevel'], capture_output=True, text=True, check=True).stdout.strip()
    jobs = int(sys.argv[1]) if len(sys.argv) > 1 else 4
    sel = [int(a) for a in sys.argv[2:]] or list(range(len(MUTS)))
    work = tempfile.mkdtemp(prefix='bench-mutations-')
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
                r = subprocess.run(['go', 'test', '-count=1', './internal/bench/'], cwd=copies[w], capture_output=True, text=True)
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
