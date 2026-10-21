# SPDX-License-Identifier: Apache-2.0
#
# The reviewer qualification harness's mutations (P31a; Opus #306; Astra #306): each is
# applied alone to a clean copy of the repository, and
# `go test ./internal/qualify/ ./internal/review/` must fail on it (KILLED).
# Run from the repository root (needs go, git, node, bwrap and the adapters
# workspace, as make test does):
#
#   python3 internal/qualify/testdata/mutations.py [jobs] [index ...]
#
# The copies keep symlinks (the adapters' node_modules): a copy that follows
# them breaks the adapter's TypeScript loading, and every run would fail.
# The copies are under the default temporary directory, never under $HOME
# (testguard fails tests there, so every mutation would read KILLED); the
# unmutated copy must pass first; and a mutation that does not compile
# (`go test -run '^$'`, which also runs go test's vet checks) is NOCOMPILE,
# never KILLED. Killed, survived and no-compile are counted apart.
import os
import shutil
import subprocess
import sys
import tempfile
from concurrent.futures import ThreadPoolExecutor

Q, R, W, P, V = 'internal/qualify/qualify.go', 'internal/qualify/route.go', 'internal/qualify/workspace.go', 'internal/review/prompt.go', 'internal/review/review.go'
MUTS = [
    # Check: the record, the latest run, the digests.
    ("another route's record counts", Q, 'if rec.Route == route {', 'if true {'),
    # The latest two runs, of different seeds (#226 entry 110).
    ("the earliest runs judged, not the latest", Q, 'return b.at.Compare(a.at)', 'return a.at.Compare(b.at)'),
    ("only the latest run judged", Q, 'if i > 1 && !r.at.Equal(runs[0].at) {', 'if i > 0 && !r.at.Equal(runs[0].at) {'),
    ("a third run at the latest time not judged", Q, 'if i > 1 && !r.at.Equal(runs[0].at) {', 'if i > 1 {'),
    ("one passing run enough", Q, 'return fmt.Errorf("%w: one run of the route', 'return nil\n\t\t_ = fmt.Errorf("%w: one run of the route'),
    ("two records of one seed count as two", Q, 'if runs[0].rec.Seed == runs[1].rec.Seed {', 'if false {'),
    ("no record passes", Q, '\tif len(runs) == 0 {\n\t\treturn fmt.Errorf("%w: no record of the route", ErrUnqualified)', '\tif len(runs) == 0 {\n\t\treturn nil'),
    ("another prompt accepted", Q, 'case rec.PromptDigest != review.PromptDigest:', 'case false:'),
    ("another adapter accepted", Q, 'case rec.AdapterDigest != s.Adapter:', 'case false:'),
    ("another suite accepted", Q, 'case rec.SuiteDigest != s.Digest:', 'case false:'),
    ("cases' count unchecked", Q, 'case len(rec.Cases) != len(s.Cases):', 'case false:'),
    ("case order unchecked", Q, 'if rc.ID != c.ID || string(rc.Expect) != c.Expect || rc.PromptDigest != c.PromptDigest {', 'if string(rc.Expect) != c.Expect || rc.PromptDigest != c.PromptDigest {'),
    ("expect unchecked", Q, 'if rc.ID != c.ID || string(rc.Expect) != c.Expect || rc.PromptDigest != c.PromptDigest {', 'if rc.ID != c.ID || rc.PromptDigest != c.PromptDigest {'),
    ("a case's prompt unchecked", Q, 'if rc.ID != c.ID || string(rc.Expect) != c.Expect || rc.PromptDigest != c.PromptDigest {', 'if rc.ID != c.ID || string(rc.Expect) != c.Expect {'),
    ("report digest unchecked", Q, 'if err != nil || digest(b) != rc.ReportDigest {', 'if err != nil {'),
    ("report not read", Q, '\t\tcase err != nil:\n\t\t\treturn fmt.Errorf("case %s\'s report is not an', '\t\tcase false:\n\t\t\treturn fmt.Errorf("case %s\'s report is not an'),
    ("report decision unchecked", Q, 'case r.Decision != decided[c.ID]:', 'case false:'),
    ("report roster unchecked", Q, 'case !slices.Equal(r.Roster, roster):', 'case len(roster) == 0:'),
    ("a call reused across cases", Q, '!slices.Equal(r.ChargeLinks, []types.CallID{ev.Call}) || calls[ev.Call]:', '!slices.Equal(r.ChargeLinks, []types.CallID{ev.Call}):'),
    ("counts unchecked", Q, 'if noGo, goes, ok := s.Score(decided); int64(noGo) != rec.NoGoOnDefects || int64(goes) != rec.GoOnControls || ok != rec.Qualified {',
     'if noGo, goes, ok := s.Score(decided); false && (int64(noGo) != rec.NoGoOnDefects || int64(goes) != rec.GoOnControls || ok != rec.Qualified) {'),
    ("under the bar accepted", Q, '\tif !rec.Qualified {\n\t\treturn fmt.Errorf("under the bar', '\tif false {\n\t\treturn fmt.Errorf("under the bar'),
    ("defect bar off", Q, 'return noGo, goes, noGo >= s.Pass.NoGo && goes >= s.Pass.Go', 'return noGo, goes, goes >= s.Pass.Go'),
    ("control bar off", Q, 'return noGo, goes, noGo >= s.Pass.NoGo && goes >= s.Pass.Go', 'return noGo, goes, noGo >= s.Pass.NoGo'),
    ("a miss counts", Q, 'if decided[c.ID] == c.Expect {', 'if decided[c.ID] == c.Expect || decided[c.ID] == "" {'),
    ("sealed files not digested", Q, 'return isDir && rel == "records" || !isDir && rel == "README.md"', 'return isDir && (rel == "records" || rel == "sealed") || !isDir && rel == "README.md"'),
    ("the adapter's source not digested", Q, 'var adapterPaths = []string{"adapters/http-chat/src", ', 'var adapterPaths = []string{'),
    ("the limits not digested", Q, 'fmt.Sprintf("max_turns=%d\\x00max_output_tokens=%d", turns, tokens)', '""'),
    # Astra #306 H1: each case's evidence binds its report to the run.
    ("evidence of another run accepted", Q, 'case ev.RunID != rec.RunID:', 'case false:'),
    ("evidence of another route accepted", Q, 'case ev.Route != rec.Route:', 'case false:'),
    ("evidence of another prompt accepted", Q, 'case ev.PromptDigest != c.PromptDigest:', 'case false:'),
    ("evidence of another adapter accepted", Q, 'case ev.AdapterDigest != rec.AdapterDigest || ev.SuiteDigest != rec.SuiteDigest:', 'case ev.SuiteDigest != rec.SuiteDigest:'),
    ("evidence of another suite accepted", Q, 'case ev.AdapterDigest != rec.AdapterDigest || ev.SuiteDigest != rec.SuiteDigest:', 'case ev.AdapterDigest != rec.AdapterDigest:'),
    ("evidence out of order accepted", Q, 'case ev.Seq != seq[c.ID]:', 'case false:'),
    ("a report charging another call than its execution's", Q, '!slices.Equal(r.ChargeLinks, []types.CallID{ev.Call}) || calls[ev.Call]:', 'len(r.ChargeLinks) == 0 || calls[ev.Call]:'),
    ("Record does not verify its outcomes", Q, '); err != nil {\n\t\treturn rec, fmt.Errorf("qualify: the outcomes are not the run', '); err != nil && false {\n\t\treturn rec, fmt.Errorf("qualify: the outcomes are not the run'),
    ("the evidence's prompt not the one sent", R, 'PromptDigest: digest(spec.Prompt),', 'PromptDigest: digest(prompt[:len(prompt)/2]),'),
    ("the evidence's route the run's claim", R, 'Route: RouteOf(e), AdapterDigest: run.Adapter,', 'Route: run.Route, AdapterDigest: run.Adapter,'),
    # Astra #306 H2: a report is read strictly.
    ("report keys loose", V, 'top, ok := exactly(v, reportKeys...)', 'top, ok := v.(map[string]any)'),
    ("roster member keys loose", V, 'm, ok := exactly(x, "executor", "family")', 'm, ok := x.(map[string]any)'),
    ("finding keys loose", V, 'm, ok := exactly(x, "severity", "title")', 'm, ok := x.(map[string]any)'),
    ("field types unchecked", V, 'case !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6:', 'case !ok1 && !ok2 && !ok3 && !ok4 && !ok5 && !ok6:'),
    ("outcome not the decision's", V, 'case !known || r.Outcome != want.outcome || want.reason != "" && reason != want.reason:', 'case !known || want.reason != "" && reason != want.reason:'),
    ("reason not the decision's", V, 'case !known || r.Outcome != want.outcome || want.reason != "" && reason != want.reason:', 'case !known || r.Outcome != want.outcome:'),
    ("an unregistered reason", V, 'if _, err := vocab.ParseReason(reason); err != nil {', 'if _, err := vocab.ParseReason(reason); false && err != nil {'),
    ("a repair round unchecked", V, 'if !ok || decision != NoGo || n != r.Round+1 {', 'if !ok {'),
    ("findings without a decision", V, '|| decision == "" && len(findings) > 0 {', '{'),
    ("a negative time or round 0", V, 'r.ElapsedMS < 0 || r.Round < 1:', 'false:'),
    ("a charge link not a call id", V, 'if !ok || ids.Check(call, "cal") != nil {', 'if !ok {'),
    # Astra #306 M3: the adapter's runtime closure.
    ("the contract's source not digested", Q, '"adapters/contract/src", ', ''),
    ("the generated validators not digested", Q, '"gen/ts/validators.cjs", ', ''),
    # Astra #306 M4: safe, bounded reads.
    ("links followed", Q, 'os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK', 'os.O_RDONLY|syscall.O_NONBLOCK'),
    ("special files read", Q, 'case !fi.Mode().IsRegular():', 'case false:'),
    ("no size check before reading", Q, 'case fi.Size() > limit:', 'case false:'),
    ("the read unbounded", Q, 'io.ReadAll(io.LimitReader(f, limit+1))', 'io.ReadAll(f)'),
    ("a file past its bound read", Q, 'if err == nil && int64(len(b)) > limit {', 'if false {'),
    ("records uncounted", Q, 'if len(entries) > maxEntries || len(names) > maxRecords {', 'if len(entries) > maxEntries {'),
    ("entries uncounted", Q, 'if len(entries) > maxEntries || len(names) > maxRecords {', 'if len(names) > maxRecords {'),
    ("a linked reports directory followed", Q, 'os.Lstat(reports)', 'os.Stat(reports)'),
    # Astra #306 M5: the seeded order.
    ("the order ignores the seed", Q, 'sha256.Sum256([]byte(seed + "\\x00" + c.ID))', 'sha256.Sum256([]byte(c.ID))'),
    ("a fixed seed", Q, '\t_, _ = rand.Read(seed)\n', ''),
    # The prompt, the workspace, the gate, the route.
    ("the prompt's drift unchecked", W, 'if err == nil && digest(p) != c.PromptDigest {', 'if false {'),
    ("the prompt digest not over the rendering", P, '\t\th.Write(p)\n', '\t\th.Write(nil)\n'),
    ("a fixed fence", P, 'fence := strings.Repeat("`", max(3, longestRun(in.Diff, \'`\')+1))', 'fence := "```"'),
    ("the gate summary dropped", P, 'strings.TrimSpace(in.Requirement), gate, fence+', 'strings.TrimSpace(in.Requirement), strings.Repeat(gate, 0), fence+'),
    ("the diff bound off", P, 'case len(in.Requirement) > maxRequirement || len(in.Diff) > maxDiff:', 'case false:'),
    ("gate always green", W, 'g.Green = err == nil && g.Failed == 0 && g.Passed > 0', 'g.Green = true'),
    ("a build failure green", W, 'g.Green = err == nil && g.Failed == 0 && g.Passed > 0', 'g.Green = g.Failed == 0'),
    ("visible overlay skipped", W, 'if v := filepath.Join(s.taskDir(c), "visible"); exists(v) {', 'if v := filepath.Join(s.taskDir(c), "visible"); false && exists(v) {'),
    ("another prompt sent", R, 'Call: call, Prompt: prompt,', 'Call: call, Prompt: []byte("Review the change."),'),
    ("decision mapping swapped", R, '\tcase types.OutcomeEmpty:\n\t\to.Decision = review.Go', '\tcase types.OutcomeEmpty:\n\t\to.Decision = review.NoGo'),
]


TESTS = ['./internal/qualify/', './internal/review/']


def main():
    repo = subprocess.run(['git', 'rev-parse', '--show-toplevel'], capture_output=True, text=True, check=True).stdout.strip()
    jobs = int(sys.argv[1]) if len(sys.argv) > 1 else 4
    sel = [int(a) for a in sys.argv[2:]] or list(range(len(MUTS)))
    work = tempfile.mkdtemp(prefix='qualify-mutations-')
    home = os.path.realpath(os.path.expanduser('~'))
    if os.path.realpath(work).startswith(home + os.sep):
        sys.exit('mutations: the copies would be under $HOME (%s): testguard fails every test there, and every mutation would read KILLED; unset TMPDIR' % work)
    copies = []
    for w in range(jobs):
        d = os.path.join(work, str(w))
        shutil.copytree(repo, d, symlinks=True, ignore=shutil.ignore_patterns('.git', '.claude', '__pycache__'))
        copies.append(d)
    for d in copies:  # the baseline: the unmutated copy passes, or no result means anything
        r = subprocess.run(['go', 'test', '-count=1'] + TESTS, cwd=d, capture_output=True, text=True)
        if r.returncode:
            shutil.rmtree(work, ignore_errors=True)
            sys.exit('mutations: the unmutated copy fails its tests:\n' + r.stdout[-4000:] + r.stderr[-4000:])

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
                # A mutation that does not compile proves nothing: it is NOCOMPILE, never KILLED.
                built = subprocess.run(['go', 'test', '-count=1', '-run', '^$'] + TESTS, cwd=copies[w], capture_output=True, text=True)
                r = built if built.returncode else subprocess.run(['go', 'test', '-count=1'] + TESTS, cwd=copies[w], capture_output=True, text=True)
            finally:
                open(p, 'w').write(src)
            out.append((i, name, 'NOCOMPILE' if built.returncode else 'KILLED' if r.returncode else 'SURVIVED'))
        return out

    with ThreadPoolExecutor(jobs) as ex:
        results = sorted(r for rs in ex.map(job, range(jobs)) for r in rs)
    for i, name, res in results:
        print(i, name, res)
    for d in copies:
        clean = subprocess.run(['diff', '-r', '-q', os.path.join(d, 'internal'), os.path.join(repo, 'internal')], capture_output=True).returncode == 0
        print('copy', os.path.basename(d), 'clean' if clean else 'DIRTY')
    counts = {k: sum(r == k for _, _, r in results) for k in ('KILLED', 'SURVIVED', 'NOCOMPILE', 'BADSTRING')}
    print(' '.join('%s %d' % kv for kv in counts.items()))
    shutil.rmtree(work, ignore_errors=True)
    sys.exit(1 if any(r != 'KILLED' for _, _, r in results) else 0)


if __name__ == '__main__':
    main()
