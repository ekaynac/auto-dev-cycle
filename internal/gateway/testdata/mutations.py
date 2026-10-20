# SPDX-License-Identifier: Apache-2.0
#
# The gateway socket serving's mutations (P22a-5g; #226 entries 57, 58, 78):
# each is applied alone to a clean copy of the repository, and
# `go test ./internal/gateway/` must fail on it (KILLED). Run from the
# repository root (needs go, node and the adapters workspace, as make test
# does):
#
#   python3 internal/gateway/testdata/mutations.py [jobs] [index ...]
#
# The copies keep symlinks (the adapters' node_modules).
import os
import shutil
import subprocess
import sys
import tempfile
from concurrent.futures import ThreadPoolExecutor

S, G = 'internal/gateway/serve.go', 'internal/gateway/gateway.go'
MUTS = [
    ("no in-flight cap", S, 'case len(g.inflight[key]) >= MaxInFlight:', 'case false:'),
    ("a slot never freed", S, 'if delete(g.inflight[key], f); len(g.inflight[key]) == 0 {', 'if len(g.inflight[key]) == 0 {'),
    ("an unbound token admitted", S, '\tcase !ok:\n\t\treturn Binding{}, &refusal{http.StatusUnauthorized, "unbound"}\n\tcase len', '\tcase false:\n\t\treturn Binding{}, &refusal{http.StatusUnauthorized, "unbound"}\n\tcase len'),
    ("no write deadline", S, '_ = f.rc.SetWriteDeadline(time.Now().Add(writeIdle))', '_ = writeIdle'),
    ("Unbind cancels nothing", G, '\tfor f := range g.inflight[key] {\n\t\tf.cancel()\n\t}', '\tfor f := range g.inflight[key] {\n\t\t_ = f\n\t}'),
    ("a minted base_url with a path", S, 'return types.AdapterPrepareParamsGatewayValue{BaseUrl: BaseURL, Token: token}, nil', 'return types.AdapterPrepareParamsGatewayValue{BaseUrl: BaseURL + "/v1", Token: token}, nil'),
    ("the request not cancellable", G, '\tf := &flight{cancel}\n\tb, no := g.enter(r, f)', '\tf := &flight{func() {}}\n\tb, no := g.enter(r, f)'),
]



def main():
    repo = subprocess.run(['git', 'rev-parse', '--show-toplevel'], capture_output=True, text=True, check=True).stdout.strip()
    jobs = int(sys.argv[1]) if len(sys.argv) > 1 else 4
    sel = [int(a) for a in sys.argv[2:]] or list(range(len(MUTS)))
    work = tempfile.mkdtemp(prefix='gateway-mutations-')
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
                r = subprocess.run(['go', 'test', '-count=1', './internal/gateway/'], cwd=copies[w], capture_output=True, text=True)
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
