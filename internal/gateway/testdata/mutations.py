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
    ("no write deadline", S, '\tarm(f.rc) // each write\'s own deadline', '\t// each write\'s own deadline'),
    ("a write's deadline left armed (Astra #308 1)", S, '\t_ = f.rc.SetWriteDeadline(time.Time{})', '\t_ = time.Time{}'),
    ("Unbind cancels nothing", G, '\tfor f := range g.inflight[key] {\n\t\tf.cancel()\n\t}', '\tfor f := range g.inflight[key] {\n\t\t_ = f\n\t}'),
    ("a minted base_url with a path", S, 'return types.AdapterPrepareParamsGatewayValue{BaseUrl: BaseURL, Token: token}, nil', 'return types.AdapterPrepareParamsGatewayValue{BaseUrl: BaseURL + "/v1", Token: token}, nil'),
    ("the request not cancellable", G, '\tf := &flight{cancel}\n\tb, no := g.enter(r, f)', '\tf := &flight{func() {}}\n\tb, no := g.enter(r, f)'),
    ("the body read before the cap refuses", G, '\tb, no := g.enter(r, f)\n\tif no != nil {\n\t\trefuse(w, rc, no)', '\tb, no := g.enter(r, f)\n\tif no != nil {\n\t\t_, _ = io.ReadAll(r.Body)\n\t\trefuse(w, rc, no)'),
    ("a capture after Unbind (Opus #322 L4)", G, '\tdone, ok := g.admit(r, f) // unbound meanwhile: nothing captured', '\tdone, ok := func() {}, true // unbound meanwhile: nothing captured'),
    ("an intent after Unbind", G, '\tif done, ok = g.admit(r, f); !ok { // unbound meanwhile: no intent', '\tif done, ok = func() {}, true; !ok { // unbound meanwhile: no intent'),
    ("a floor after Unbind", G, '\tif done, ok = g.admit(r, f); !ok { // unbound meanwhile: no floor', '\tif done, ok = func() {}, true; !ok { // unbound meanwhile: no floor'),
    ("Unbind returns before what it admitted (Astra #308 2)", G, '\t\tops.Wait()\n', ''),
    ("a side effect admitted after Unbind (Astra #308 2)", S, '\tif !g.inflight[key][f] || r.Context().Err() != nil {\n\t\treturn nil, false', '\tif r.Context().Err() != nil && false {\n\t\treturn nil, false'),
    ("a slot held through its count", G, '\trelease() // the upstream exchange is over', '\t// the upstream exchange is over'),
    ("the response's end under an old deadline (Astra #308 1)", G, '\tdefer arm(rc) // the response', '\t// the response'),
    ("a refusal without its deadline (Astra #308 3)", G, '\tarm(rc)       // nothing is written', '\t// nothing is written'),
    ("an unsupported deadline written past (Astra #308 3)", S, '\t\tpanic(http.ErrAbortHandler)\n', ''),
    ("a refusal draining the body (Astra #308 4)", G, '\th.Set("Connection", "close")\n', ''),
    ("a refused connection kept reading (Astra #308 4)", G, '\t_ = rc.SetReadDeadline(time.Now())', '\t_ = time.Now'),
    ("no server write timeout", S, 'WriteTimeout: RequestDeadline + time.Minute, ', ''),
    ("no server read timeouts", S, 'ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: time.Minute,', ''),
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
                r = subprocess.run(['go', 'test', '-count=1', '-timeout', '120s', './internal/gateway/'], cwd=copies[w], capture_output=True, text=True)
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
