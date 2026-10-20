# SPDX-License-Identifier: Apache-2.0
#
# The reviewer qualification harness's mutations (P31a; Opus #306): each is
# applied alone to a clean copy of the repository, and
# `go test ./internal/qualify/ ./internal/review/` must fail on it (KILLED).
# Run from the repository root (needs go, git, node, bwrap and the adapters
# workspace, as make test does):
#
#   python3 internal/qualify/testdata/mutations.py [jobs] [index ...]
#
# The copies keep symlinks (the adapters' node_modules): a copy that follows
# them breaks the adapter's TypeScript loading, and every run would fail.
import os
import shutil
import subprocess
import sys
import tempfile
from concurrent.futures import ThreadPoolExecutor

Q, R, W, P = 'internal/qualify/qualify.go', 'internal/qualify/route.go', 'internal/qualify/workspace.go', 'internal/review/prompt.go'
MUTS = [
    # Check: the record, the latest run, the digests.
    ("another route's record counts", Q, 'if rec.Route == route {', 'if true {'),
    ("an older run judged, not the latest", Q, 'if !r.at.Equal(latest) {', 'if r.at.Equal(latest) {'),
    ("no record passes", Q, 'return fmt.Errorf("%w: no record of the route", ErrUnqualified)', 'return nil'),
    ("another prompt accepted", Q, 'case rec.PromptDigest != review.PromptDigest:', 'case false:'),
    ("another adapter accepted", Q, 'case rec.AdapterDigest != s.Adapter:', 'case false:'),
    ("another suite accepted", Q, 'case rec.SuiteDigest != s.Digest:', 'case false:'),
    ("cases' count unchecked", Q, 'case len(rec.Cases) != len(s.Cases):', 'case false:'),
    ("case order unchecked", Q, 'if rc.ID != c.ID || string(rc.Expect) != c.Expect || rc.PromptDigest != c.PromptDigest {', 'if string(rc.Expect) != c.Expect || rc.PromptDigest != c.PromptDigest {'),
    ("expect unchecked", Q, 'if rc.ID != c.ID || string(rc.Expect) != c.Expect || rc.PromptDigest != c.PromptDigest {', 'if rc.ID != c.ID || rc.PromptDigest != c.PromptDigest {'),
    ("a case's prompt unchecked", Q, 'if rc.ID != c.ID || string(rc.Expect) != c.Expect || rc.PromptDigest != c.PromptDigest {', 'if rc.ID != c.ID || string(rc.Expect) != c.Expect {'),
    ("report digest unchecked", Q, 'if err != nil || digest(b) != rc.ReportDigest {', 'if err != nil {'),
    ("report not read", Q, 'case err != nil || r.Schema != reportSchema:', 'case false:'),
    ("report keys not required", Q, '\tfor _, k := range reportKeys {\n\t\tif _, ok := m[k]; !ok {', '\tfor _, k := range reportKeys[:0] {\n\t\tif _, ok := m[k]; !ok {'),
    ("report decision unchecked", Q, 'case r.Decision != decided[c.ID] || r.Outcome != outcomeOf[r.Decision]:', 'case r.Outcome != outcomeOf[r.Decision]:'),
    ("report outcome unchecked", Q, 'case r.Decision != decided[c.ID] || r.Outcome != outcomeOf[r.Decision]:', 'case r.Decision != decided[c.ID]:'),
    ("report roster unchecked", Q, 'case !slices.Equal(r.Roster, roster):', 'case false:'),
    ("a call reused across cases", Q, 'slices.ContainsFunc(r.ChargeLinks, func(l types.CallID) bool { return links[l] })', 'false'),
    ("counts unchecked", Q, 'case int64(noGo) != rec.NoGoOnDefects || int64(goes) != rec.GoOnControls || ok != rec.Qualified:', 'case false:'),
    ("under the bar accepted", Q, '\tcase !ok:\n\t\treturn fmt.Errorf("under the bar', '\tcase false:\n\t\treturn fmt.Errorf("under the bar'),
    ("defect bar off", Q, 'return noGo, goes, noGo >= s.Pass.NoGo && goes >= s.Pass.Go', 'return noGo, goes, goes >= s.Pass.Go'),
    ("control bar off", Q, 'return noGo, goes, noGo >= s.Pass.NoGo && goes >= s.Pass.Go', 'return noGo, goes, noGo >= s.Pass.NoGo'),
    ("a miss counts", Q, 'if decided[c.ID] == c.Expect {', 'if decided[c.ID] == c.Expect || decided[c.ID] == "" {'),
    ("sealed files not digested", Q, 'return isDir && rel == "records" || !isDir && rel == "README.md"', 'return isDir && (rel == "records" || rel == "sealed") || !isDir && rel == "README.md"'),
    ("the adapter's source not digested", Q, 'var adapterPaths = []string{"adapters/http-chat/src", ', 'var adapterPaths = []string{'),
    ("the limits not digested", Q, 'fmt.Sprintf("max_turns=%d\\x00max_output_tokens=%d", turns, tokens)', '""'),
    # The prompt, the workspace, the gate, the route.
    ("the prompt's drift unchecked", W, 'if err == nil && digest(p) != c.PromptDigest {', 'if false {'),
    ("the prompt digest not over the rendering", P, '\t\th.Write(p)\n', '\t\th.Write(nil)\n'),
    ("a fixed fence", P, 'fence := strings.Repeat("`", max(3, longestRun(in.Diff, \'`\')+1))', 'fence := "```"'),
    ("the gate summary dropped", P, 'strings.TrimSpace(in.Requirement), gate, fence+', 'strings.TrimSpace(in.Requirement), "", fence+'),
    ("the diff bound off", P, 'case len(in.Requirement) > maxRequirement || len(in.Diff) > maxDiff:', 'case false:'),
    ("gate always green", W, 'g.Green = err == nil && g.Failed == 0 && g.Passed > 0', 'g.Green = true'),
    ("a build failure green", W, 'g.Green = err == nil && g.Failed == 0 && g.Passed > 0', 'g.Green = g.Failed == 0'),
    ("visible overlay skipped", W, 'if v := filepath.Join(s.taskDir(c), "visible"); exists(v) {', 'if v := filepath.Join(s.taskDir(c), "visible"); false && exists(v) {'),
    ("another prompt sent", R, 'Call: call, Prompt: prompt,', 'Call: call, Prompt: []byte("Review the change."),'),
    ("decision mapping swapped", R, '\tcase types.OutcomeEmpty:\n\t\to.Decision = review.Go', '\tcase types.OutcomeEmpty:\n\t\to.Decision = review.NoGo'),
]


def main():
    repo = subprocess.run(['git', 'rev-parse', '--show-toplevel'], capture_output=True, text=True, check=True).stdout.strip()
    jobs = int(sys.argv[1]) if len(sys.argv) > 1 else 4
    sel = [int(a) for a in sys.argv[2:]] or list(range(len(MUTS)))
    work = tempfile.mkdtemp(prefix='qualify-mutations-')
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
                r = subprocess.run(['go', 'test', '-count=1', './internal/qualify/', './internal/review/'], cwd=copies[w], capture_output=True, text=True)
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
