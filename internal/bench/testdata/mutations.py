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

B, C, S = 'internal/bench/bench.go', 'internal/bench/compare.go', 'schemas/records/benchmark-run.json'
MUTS = [
    ("an unattributed measurement dropped", B, '\t\tif !ok {\n\t\t\treturn bt, fmt.Errorf("measurement %s of attempt %s, which is no node\'s", u.ID, u.Attempt)', '\t\tif !ok {\n\t\t\tcontinue'),
    ("an attempt of two nodes accepted", B, 'if _, dup := nodeOf[a]; dup {', 'if false {'),
    ("an unknown stage accepted", B, 'if !slices.Contains(Stages, n.Stage) {', 'if false {'),
    ("done before claimed accepted", B, '} else if !n.Done.IsZero() && n.Done.Before(n.Claimed) {', '} else if false {'),
    ("closed before approval accepted", B, 'case !t.Closed.IsZero() && t.Closed.Before(t.Approved):', 'case false:'),
    ("an unsettled call settled", B, '\t\tif !standing.Settled {\n\t\t\tinto = outstanding', '\t\tif false {\n\t\t\tinto = outstanding'),
    ("an unfinished node counted", B, '\t\tif n.Done.IsZero() {\n\t\t\tst.Unfinished++', '\t\tif false {\n\t\t\tst.Unfinished++'),
    ("rounds as nodes", B, 'used, rounds[n.Round] = true, true', 'used, rounds[len(rounds)] = true, true'),
    ("cache tokens dropped", B, 'st.Tokens.CacheRead, st.Tokens.CacheWrite = st.Tokens.CacheRead+f.CacheRead, st.Tokens.CacheWrite+f.CacheWrite', 'st.Tokens.CacheRead, st.Tokens.CacheWrite = 0, 0'),
    ("rescues as interventions", B, 'if iv.Stage != s || iv.Kind == Rescue {', 'if iv.Stage != s {'),
    ("rescues not counted", B, 'bt.Rescues += b2i(iv.Kind == Rescue)', 'bt.Rescues += 0'),
    ("a hold dropped", B, '\t\th := t.Hold\n\t\tbt.Hold = &h', '\t\th := t.Hold\n\t\t_ = h'),
    ("an unknown intervention accepted", B, 'if !slices.Contains(kinds, iv.Kind) {', 'if false {'),
    ("a changed outcome unreported", C, 'if a, b := verdict(p), verdict(t); a != b {', 'if false {'),
    ("a lower metric reported", C, '\t\t\t\tif m.is > m.was {', '\t\t\t\tif m.is != m.was {'),
    ("a task gone unreported", C, '\t\tif !seen[t.Task] {', '\t\tif false {'),
    ("wall time growth unreported", C, 'if p.WallMs != nil && t.WallMs != nil && *t.WallMs > *p.WallMs {', 'if false {'),
    # The review of #313 (docs/reviews/2026-10-20-pr313-opus.md): H1.
    ("settled by Uncertain, not the standing", B, 'if !standing.Settled {', 'if calls[id].Uncertain {'),
    ("tokens without the gateway's count", B, 'f := *standing.Usage', 'f := calls[id].Usage'),
    ("an unsettled call not priced", B, 'micros, currency = max(micros, priced), "USD"', 'currency = cmp.Or(currency, "USD")'),
    ("a reported cost below the price kept", B, 'micros, currency = max(micros, priced), "USD"', 'micros, currency = cmp.Or(micros, priced), "USD"'),
    ("an empty currency in the schema", S, '"pattern": "^[A-Z]{3}$"', '"pattern": "^([A-Z]{3})?$"'),
    # M1.
    ("a call in two stages accepted", B, 'ok && t.Nodes[j].Stage != t.Nodes[i].Stage {', 'false {'),
    ("a call on two executors accepted", B, '} else if ok && t.Nodes[j].Executor != t.Nodes[i].Executor {', '} else if false {'),
    # M2.
    ("an empty run accepted", B, 'if len(tasks) == 0 {', 'if false {'),
    ("ids twice in the run accepted", B, '\t\tif seen[k] {', '\t\tif false {'),
    ("no approval time accepted", B, 'case t.Approved.IsZero():', 'case false:'),
    ("closed and held accepted", B, 'case !t.Closed.IsZero() && t.Hold != "":', 'case false:'),
    ("neither closed nor held accepted", B, 'case t.Closed.IsZero() && t.Hold == "":', 'case false:'),
    ("closed without an outcome accepted", B, 'case !t.Closed.IsZero() && (t.Outcome == "" || t.Reason.String() == ""):', 'case false:'),
    ("closed without a reason accepted", B, '(t.Outcome == "" || t.Reason.String() == "")', '(t.Outcome == "")'),
    ("an unknown hold accepted", B, 'if _, ok := reg.Holds[t.Hold]; !ok {', 'if false {'),
    ("an intervention at an unknown stage accepted", B, '} else if iv.Kind != Rescue && !slices.Contains(Stages, iv.Stage) {', '} else if false {'),
    ("no claimed time accepted", B, '} else if n.Claimed.IsZero() {', '} else if false {'),
    ("no route's executor accepted", B, 'len(n.Attempts) > 0 && !ok {', 'false {'),
    ("a route without a price accepted", B, 'if _, ok := m.Prices[r.Executor]; !ok {', 'if false {'),
    ("the output not validated", B, '_, err = contract.Decode[types.BenchmarkRun]("records/benchmark-run", raw)', '_ = raw'),
    # M3.
    ("the gateway's basis counted", B, '\t\t\tat = gateway\n', '\t\t\tat = own\n'),
    ("two bases accepted", B, 'if len(own) != 1 {', 'if len(own) == 0 {'),
    # Opus's extra mutations, and the Lows.
    ("approvals not counted", B, 'st.Interventions.Approvals++', 'st.Interventions.Approvals += 0'),
    ("basis ignored", B, 'into[k] = &types.BenchmarkAmount{Basis: basis, Currency: currency}', 'into[k] = &types.BenchmarkAmount{Basis: "cash", Currency: currency}'),
    ("a stage of interventions only dropped", B, 'switch used = true; iv.Kind {', 'switch iv.Kind {'),
    ("recorded_at not in UTC", B, 'm.RecordedAt.UTC().Format', 'm.RecordedAt.Format'),
    ("rescues not compared", C, 'if t.Rescues > p.Rescues {', 'if false {'),
    ("outstanding not compared", C, 'out = append(out, amountsGrew(t.Task, s.Stage, "outstanding_micros", q.Outstanding, s.Outstanding)...)', ''),
    ("charge not compared", C, 'out = append(out, amountsGrew(t.Task, s.Stage, "charge_micros", q.Charge, s.Charge)...)', ''),
    ("interventions not compared", C, '{"interventions", interventions(q), interventions(s)},', ''),
    ("attempts not compared", C, '{"attempts", q.Attempts, s.Attempts}, ', ''),
    ("rounds not compared", C, '{"rounds", q.Rounds, s.Rounds},', ''),
    ("unfinished not compared", C, '{"unfinished", q.Unfinished, s.Unfinished}, ', ''),
    ("amounts summed across currencies", C, 'w[string(a.Basis)+" "+a.Currency] = a.AmountMicros', 'w[string(a.Basis)] += a.AmountMicros'),
    ("a cell not escaped", C, 'return strings.ReplaceAll(s, "|", `\\|`)', 'return s'),
    ("the path with a slash", C, 'strings.ReplaceAll(run.Arch, "/", "-")', 'run.Arch'),
    ("unfinished not summarised", C, 'seconds(s.WallMs), s.Unfinished,', 'seconds(s.WallMs), int64(0),'),
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
        clean = all(subprocess.run(['diff', '-r', '-q', os.path.join(d, x), os.path.join(repo, x)], capture_output=True).returncode == 0 for x in ('internal', 'schemas'))
        print('copy', os.path.basename(d), 'clean' if clean else 'DIRTY')
    shutil.rmtree(work, ignore_errors=True)
    sys.exit(1 if any(r != 'KILLED' for _, _, r in results) else 0)


if __name__ == '__main__':
    main()
