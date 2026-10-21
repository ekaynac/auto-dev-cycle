# SPDX-License-Identifier: Apache-2.0
#
# The benchmark recorder's mutations (P25): each is applied alone to a
# clean copy of the repository, which must build (`go vet`; otherwise
# NOCOMPILE, not a kill), and `go test ./internal/bench/` must fail on it
# (KILLED). The copy is under /tmp, never under $HOME, and TMPDIR is
# ignored. Run from the repository root:
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
    ("an attempt of two nodes accepted", B, '} else if _, dup := nodeOf[a.ID]; dup {', '} else if false {'),
    ("an unknown stage accepted", B, 'if !slices.Contains(Stages, n.Stage) {', 'if false {'),
    ("done before claimed accepted", B, '} else if !n.Done.IsZero() && n.Done.Before(n.Claimed) {', '} else if false {'),
    ("closed before approval accepted", B, 'case !t.Closed.IsZero() && t.Closed.Before(t.Approved):', 'case false:'),
    ("an unsettled call settled", B, '\t\t\t\tbasis = "cash"\n\t\t\t\tif c == "" || c == "USD" {', '\t\t\t\tbasis, into, owed = "cash", settled, false\n\t\t\t\tif c == "" || c == "USD" {'),
    ("an unfinished node counted", B, '\t\tif n.Done.IsZero() {\n\t\t\tst.Unfinished++', '\t\tif false {\n\t\t\tst.Unfinished++'),
    ("rounds as nodes", B, 'rounds[n.Round] = true', 'rounds[len(rounds)] = true'),
    ("cache reads dropped", B, 'add(&err, &st.Tokens.CacheRead, f.CacheRead)', ''),
    ("cache writes dropped", B, 'add(&err, &st.Tokens.CacheWrite, f.CacheWrite)', ''),
    ("rescues as interventions", B, 'if iv.Stage != s || iv.Kind == Rescue {', 'if iv.Stage != s {'),
    ("rescues not counted", B, 'bt.Rescues += b2i(iv.Kind == Rescue)', 'bt.Rescues += 0'),
    ("a hold dropped", B, '\t\th := t.Hold\n\t\tbt.Hold = &h', '\t\th := t.Hold\n\t\t_ = h'),
    ("an unknown intervention accepted", B, 'if !slices.Contains(kinds, iv.Kind) {', 'if false {'),
    ("a changed outcome unreported", C, 'if a, b := verdict(p), verdict(t); a != b {', 'if a, b := verdict(p), verdict(t); a != b && false {'),
    ("a lower metric reported", C, '\t\t\t\tif m.is > m.was {', '\t\t\t\tif m.is != m.was {'),
    ("a task gone unreported", C, '\t\tif !seen[t.Task] {', '\t\tif false {'),
    ("wall time growth unreported", C, 'if p.WallMs != nil && t.WallMs != nil && *t.WallMs > *p.WallMs {', 'if false {'),
    # The review of #313 (docs/reviews/2026-10-20-pr313-opus.md): H1.
    ("settled by Uncertain, not the standing", B, 'case standing.Settled:\n', 'case !calls[id].Uncertain:\n'),
    ("tokens without the gateway's count", B, 'f := *standing.Usage', 'f := calls[id].Usage'),
    ("an unsettled call not priced", B, 'micros = max(micros, priced)', ''),
    ("a reported cost below the price kept", B, 'micros = max(micros, priced)', 'micros = cmp.Or(micros, priced)'),
    ("an empty currency in the schema", S, '"pattern": "^[A-Z]{3}$"', '"pattern": "^([A-Z]{3})?$"'),
    # M1.
    ("a call in two stages accepted", B, '; s != t.Nodes[i].Stage {', '; false {'),
    ("a call on two executors accepted", B, '} else if exec[ca] != exec[u.Attempt] {', '} else if false {'),
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
    ("no claimed time accepted", B, '} else if n.Claimed.IsZero() && (', '} else if false && ('),
    ("no route's executor accepted", B, 'if _, ok := route[a.Executor]; !ok {', 'if false {'),
    ("a route without a price accepted", B, 'if _, ok := m.Prices[r.Executor]; !ok {', 'if false {'),
    ("the output not validated", B, '_, err = contract.Decode[types.BenchmarkRun]("records/benchmark-run", raw)', '_, _ = contract.Decode[types.BenchmarkRun]("records/benchmark-run", raw)'),
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
    ("unfinished not summarised", C, 'unfinished := fmt.Sprint(s.Unfinished)', 'unfinished := "0"'),
    # The review of #313 by Astra (docs/reviews/2026-10-21-pr313-astra.md).
    # H1.
    ("an unsettled call at zero accepted", B, 'if owed && micros <= 0 {', 'if owed && false {'),
    ("the liability ignored", B, 'liability[id], "USD"', 'int64(0), "USD"'),
    # H2.
    ("unmeasured bound calls dropped", B, 'for _, id := range slices.Sorted(slices.Values(bound)) {', 'for _, id := range slices.Sorted(maps.Keys(calls)) {'),
    ("a stage of unmeasured calls dropped", B, 'if err != nil || len(bound) == 0 {', 'if err != nil || len(usage) == 0 {'),
    ("an unbound measurement accepted", B, '\t\tif !ok {\n\t\t\treturn bt, fmt.Errorf("measurement %s of call %s', '\t\tif false {\n\t\t\treturn bt, fmt.Errorf("measurement %s of call %s'),
    ("a bound call of no node's attempt accepted", B, '\t\tif !ok {\n\t\t\treturn bt, fmt.Errorf("bound call', '\t\tif !ok && false {\n\t\t\treturn bt, fmt.Errorf("bound call'),
    ("a call bound twice accepted", B, '} else if _, dup := callAttempt[c.ID]; dup {', '} else if false {'),
    ("a negative liability accepted", B, '} else if c.Liability < 0 {', '} else if false {'),
    ("no sources as null", B, 'st.Sources = append(st.Sources, slices.Sorted(maps.Keys(sources))...)', 'st.Sources = slices.Sorted(maps.Keys(sources))'),
    # H3.
    ("an unsettled non-USD amount kept", B, 'case c != "" && c != "USD" && reported != 0:', 'case false:'),
    # H4.
    ("a sum unchecked", B, 'v > maxSum-*dst', 'false'),
    ("tokens added unchecked", B, 'add(&err, &st.Tokens.In, f.In)', 'st.Tokens.In += f.In'),
    ("money added unchecked", B, 'add(&err, &into[k].AmountMicros, micros)', 'into[k].AmountMicros += micros'),
    # M5.
    ("a price for a route", B, 'route[a.Executor]; !ok', 'm.Prices[a.Executor]; !ok'),
    ("a conflicting route accepted", B, 'ok && q != r {', 'ok && q != r && false {'),
    # Opus's re-check of #313 (docs/reviews/2026-10-21-pr313-opus-r2.md).
    # M1.
    ("a settled cash call not floored", B, 'max(reported, priced), c', 'reported, c'),
    ("a settled non-USD call kept at a positive floor", B, 'c != "USD" && priced > 0:', 'c != "USD" && false:'),
    # M3.
    ("pending not counted", B, 'st.Pending++\n', '\n'),
    ("pending counted as unfinished", B, '\t\t\tst.Pending++\n\t\t\tcontinue\n', '\t\t\tst.Pending++\n'),
    ("pending not compared", C, '{"pending", q.Pending, s.Pending}, ', ''),
    ("pending not summarised", C, 'if s.Pending > 0 {', 'if false {'),
    # L1.
    ("a task after the recording accepted", B, 'case t.Approved.After(m.RecordedAt) || t.Closed.After(m.RecordedAt):', 'case false:'),
    ("a node after the recording accepted", B, '} else if n.Claimed.After(m.RecordedAt) || n.Done.After(m.RecordedAt) {', '} else if false {'),
    ("a saturated duration accepted", B, 'd == time.Duration(math.MaxInt64)', 'd == time.Duration(math.MaxInt64) && false'),
    ("a node's saturated duration accepted", B, 'err = cmp.Or(err, serr)', '_ = serr'),
    # L2.
    ("a negative cost accepted", B, '} else if u.Cost.AmountMicros < 0 {', '} else if false {'),
    # L3.
    ("a node's first executor prices its calls", B, 'price[c] = m.Prices[exec[a]]', 'price[c] = m.Prices[t.Nodes[nodeOf[a]].Attempts[0].Executor]'),
    # The review of #313 by Mythos (docs/reviews/2026-10-21-pr313-mythos.md).
    # H1.
    ("the floor skipped off cash", B, '\t\t\tcase standing.Settled:\n', '\t\t\tcase standing.Settled && basis != "cash":\n\t\t\t\tinto, owed, micros, currency = settled, false, reported, c\n\t\t\tcase standing.Settled:\n'),
    ("a settled non-USD call refused at a zero floor", B, 'c != "USD" && priced > 0:', 'c != "USD":'),
    # L2.
    ("an unsettled floor under its reported basis", B, '\t\t\t\tbasis = "cash"\n', ''),
    # The re-review of #313 by Mythos (docs/reviews/2026-10-21-pr313-mythos-r2.md): L1.
    ("a floored charge under its reported basis", B, 'if priced > reported {', 'if false {'),
    ("every settled charge under cash", B, 'if priced > reported {', 'if true {'),
]



def main():
    repo = subprocess.run(['git', 'rev-parse', '--show-toplevel'], capture_output=True, text=True, check=True).stdout.strip()
    jobs = int(sys.argv[1]) if len(sys.argv) > 1 else 4
    sel = [int(a) for a in sys.argv[2:]] or list(range(len(MUTS)))
    env = {k: v for k, v in os.environ.items() if k != 'TMPDIR'}
    work = tempfile.mkdtemp(prefix='bench-mutations-', dir='/tmp')
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
                v = subprocess.run(['go', 'vet', './internal/bench/'], cwd=copies[w], capture_output=True, text=True, env=env)
                r = v if v.returncode else subprocess.run(['go', 'test', '-count=1', './internal/bench/'], cwd=copies[w], capture_output=True, text=True, env=env)
            finally:
                open(p, 'w').write(src)
            out.append((i, name, 'NOCOMPILE' if v.returncode else 'KILLED' if r.returncode else 'SURVIVED'))
        return out

    with ThreadPoolExecutor(jobs) as ex:
        results = sorted(r for rs in ex.map(job, range(jobs)) for r in rs)
    for i, name, res in results:
        print(i, name, res)
    print('killed', sum(r == 'KILLED' for _, _, r in results), 'survived', sum(r == 'SURVIVED' for _, _, r in results),
          'nocompile', sum(r == 'NOCOMPILE' for _, _, r in results), 'badstring', sum(r == 'BADSTRING' for _, _, r in results))
    for d in copies:
        clean = all(subprocess.run(['diff', '-r', '-q', os.path.join(d, x), os.path.join(repo, x)], capture_output=True).returncode == 0 for x in ('internal', 'schemas'))
        print('copy', os.path.basename(d), 'clean' if clean else 'DIRTY')
    shutil.rmtree(work, ignore_errors=True)
    sys.exit(1 if any(r != 'KILLED' for _, _, r in results) else 0)


if __name__ == '__main__':
    main()
