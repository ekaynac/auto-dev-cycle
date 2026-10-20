// SPDX-License-Identifier: Apache-2.0

//go:build corpus

package accept

// The Python guard's false-block measurement (P14c-2a-widen-b, D55): for each
// repository of testdata/pycorpus.tsv, at its pinned commit, the base's
// verdict, each file the base closure reads judged by the inertness rules,
// and each judged file's import-time findings (Opus #266's measure). It only
// reads the corpus: each repository is cloned into a temporary directory
// first. Run:
//
//	ADC_PY_CORPUS_ROOT=<dir> ADC_PY_CORPUS_INFRAMEDIC=<dir> ADC_PY_CORPUS_OUT=<dir> \
//	  go test -tags corpus -run TestPyCorpus -timeout 2h ./internal/accept/

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

type corpusRepo struct{ name, url, tag, commit, dir string }

// corpusFile is one file the base closure reads, or a judged file.
type corpusFile struct {
	Repo, Path   string
	Judged       bool
	Inert        bool     // no kind below
	Kinds        []string // why it is not inert, each once
	Details      []string // "kind\x00detail": the callee, decorator, base or dunder of a kind
	MainKinds    []string // the kinds main's allowlist (pyAllowed) rejects too
	Finds        int      // pyScan's import-time findings (Opus #266's measure), judged files only
	StrictFinds  int
	FindKinds    []string
	Disagreement bool // the classifier and pyBase.file disagree on impurity
}

// corpusRepoResult is one repository's verdict.
type corpusRepoResult struct {
	Name, Commit                  string
	Files, PyFiles, Judged, Read  int
	BaseUnread                    string // custom, the install reader or sysUnread, then holderUnread
	Custom                        string // the configuration file and the setting that make it custom
	Closure                       string // the base closure's own first unread reason (pyBase.holderUnread)
	Verdict                       []string
	Seconds                       float64
	JudgedNotInert, ReadNotInert  int
	JudgedWithFinds, JudgedStrict int
}

func TestPyCorpus(t *testing.T) {
	root, out := os.Getenv("ADC_PY_CORPUS_ROOT"), os.Getenv("ADC_PY_CORPUS_OUT")
	if root == "" || out == "" {
		t.Skip("set ADC_PY_CORPUS_ROOT and ADC_PY_CORPUS_OUT")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	var results []corpusRepoResult
	var files []corpusFile
	for _, c := range readCorpus(t, root) {
		res, fs := measureCorpus(t, c)
		results, files = append(results, res), append(files, fs...)
	}
	j, err := os.Create(filepath.Join(out, "files.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	enc := json.NewEncoder(j)
	for _, f := range files {
		if err := enc.Encode(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(out, "summary.md"), []byte(corpusSummary(results, files)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readCorpus(t *testing.T, root string) (repos []corpusRepo) {
	f, err := os.Open("testdata/pycorpus.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for s := bufio.NewScanner(f); s.Scan(); {
		if l := s.Text(); l == "" || strings.HasPrefix(l, "#") {
			continue
		} else if c := strings.Split(l, "\t"); len(c) != 5 {
			t.Fatalf("pycorpus.tsv: %q", l)
		} else {
			dir := filepath.Join(root, c[4])
			if env, ok := strings.CutPrefix(c[4], "$"); ok {
				if dir = os.Getenv(env); dir == "" {
					t.Fatalf("%s names no directory", env)
				}
			}
			repos = append(repos, corpusRepo{c[0], c[1], c[2], c[3], dir})
		}
	}
	return repos
}

// measureCorpus clones c into a temporary directory (the corpus is only
// read), then reads its base as the guard does.
func measureCorpus(t *testing.T, c corpusRepo) (corpusRepoResult, []corpusFile) {
	ctx, start := context.Background(), time.Now()
	scratch := filepath.Join(t.TempDir(), "s")
	plain(t, filepath.Dir(scratch), "clone", "--quiet", "--no-hardlinks", "--no-checkout", c.dir, scratch)
	plain(t, scratch, "branch", "-f", "corpus", c.commit)
	repo, err := sandbox.Host{Root: t.TempDir()}.Clone(ctx, scratch, "refs/heads/corpus", filepath.Join(t.TempDir(), "ctl"))
	if err != nil {
		t.Fatal(err)
	}
	g, cleanup, err := newIndex(ctx, repo, c.commit)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	r := &run{ctx: ctx, repo: repo, base: c.commit, read: g.blob, budget: maxRead, blobs: map[string][]byte{}, cgoBlobs: map[string][]byte{}, touched: map[string]bool{}}
	if err := r.list(); err != nil || r.files == nil {
		t.Fatalf("%s: the base list: %v %q", c.name, err, whats(r.blocks))
	}
	r.cand = maps.Clone(r.files)
	r.baseDirs, r.candDirs = byDir(r.files), byDir(r.cand)
	custom, _, paths, err := r.pyCustom()
	if err != nil {
		t.Fatal(err)
	}
	installed, unread, err := r.pyInstall()
	if err != nil {
		t.Fatal(err)
	}
	pb := &pyBase{r: r, extra: installed, changed: map[string]bool{}}
	pb.entries, pb.basedirs = r.pyEntries(paths)
	if err := pb.load(); err != nil {
		t.Fatal(err)
	}
	res := corpusRepoResult{Name: c.name, Commit: c.commit, Files: len(r.files), Judged: len(pb.tests), Read: len(pb.files)}
	for p := range r.files {
		res.PyFiles += b2i(path.Ext(p) == ".py")
	}
	if res.Closure = pb.holderUnread; custom {
		res.Custom = corpusCustom(t, r)
	}
	res.BaseUnread = cmp.Or(map[bool]string{true: "custom pytest configuration"}[custom], map[bool]string{true: "an install key or sys.path entry unread"}[unread || pb.sysUnread], pb.holderUnread)
	var out []corpusFile
	for _, q := range slices.Sorted(maps.Keys(pb.files)) {
		f := pb.files[q]
		cf := corpusFile{Repo: c.name, Path: q, Judged: slices.Contains(pb.tests, f)}
		cf.Kinds, cf.MainKinds, cf.Details, cf.Disagreement = corpusKinds(t, pb, q, f)
		if cf.Inert = len(cf.Kinds) == 0; cf.Judged {
			fa, ok, err := r.pyScan(true, q, r.files[q], true)
			if err != nil {
				t.Fatal(err)
			} else if ok {
				for _, fd := range fa.finds {
					cf.Finds, cf.StrictFinds = cf.Finds+1, cf.StrictFinds+b2i(fd.strict)
					cf.FindKinds = append(cf.FindKinds, corpusWhy(fd.why))
				}
				slices.Sort(cf.FindKinds)
				cf.FindKinds = slices.Compact(cf.FindKinds)
			}
			res.JudgedNotInert += b2i(!cf.Inert)
			res.JudgedWithFinds += b2i(cf.Finds > 0)
			res.JudgedStrict += b2i(cf.StrictFinds > 0)
		}
		res.ReadNotInert += b2i(!cf.Inert)
		out = append(out, cf)
	}
	if len(pb.tests) > 0 { // the guard's verdict for a comment line added to the first judged file
		var judged []string
		for _, f := range pb.tests {
			judged = append(judged, f.path)
		}
		q := slices.Min(judged)
		b, _, err := g.blob(ctx, r.files[q], maxFile)
		if err != nil {
			t.Fatal(err)
		}
		edited := append(append(slices.Clone(b), "\n"[:b2i(len(b) > 0 && b[len(b)-1] != '\n')]...), "# widen-b corpus probe\n"...)
		sum := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(edited))), edited...))
		fake := hex.EncodeToString(sum[:])
		read := func(ctx context.Context, id string, max int64) ([]byte, bool, error) {
			if id == fake {
				return edited, int64(len(edited)) <= max, nil
			}
			return g.blob(ctx, id, max)
		}
		blocks, _, err := Integrity(repo, c.commit, nil, "")(ctx, []Change{{SrcMode: "100644", DstMode: "100644", SrcBlob: r.files[q], DstBlob: fake, Status: "M", Src: q, Dst: q}}, read)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range pyOwn(blocks) {
			res.Verdict = append(res.Verdict, b.Path+": "+b.What)
		}
	}
	res.Seconds = time.Since(start).Seconds()
	t.Logf("%s: %d files, %d judged, %d read, base %q, %.1fs", c.name, res.Files, res.Judged, res.Read, res.BaseUnread, res.Seconds)
	return res, out
}

// corpusKinds: why f is not inert, each kind once, and the kinds main's
// allowlist rejects too. The statements are pyBase.file's: module level,
// class bodies and decorators, not def bodies or the exact __main__ guard.
func corpusKinds(t *testing.T, pb *pyBase, q string, f *pyBaseFile) (kinds, mains, details []string, disagree bool) {
	add := func(to *[]string, k string) {
		if !slices.Contains(*to, k) {
			*to = append(*to, k)
		}
	}
	switch {
	case f.unread:
		add(&kinds, "risky: unread (encoding, tokens, size or a link)")
	case f.runs:
		add(&kinds, "risky: a dynamic import or exec")
	}
	if f.blunt != "" {
		add(&kinds, "risky: "+corpusWhy(f.blunt))
	}
	if f.active {
		add(&kinds, "risky: a write word (setattr, __dict__, vars, globals, …)")
	}
	if len(f.writes) > 0 {
		add(&kinds, "risky: an attribute, subscript or del target")
	}
	if f.sysUnread {
		add(&kinds, "risky: a sys use beyond the allowlist")
	}
	if why, err := pb.starsOf(q, f); err != nil {
		t.Fatal(err)
	} else if why != "" {
		add(&kinds, "risky: a package star not proved")
	}
	for _, m := range f.argMods {
		if cmp.Or(pb.mods[m], pb.shadowMods[m]) == "" {
			add(&kinds, "risky: a decorator argument from outside the repository")
		}
	}
	if f.unread {
		return kinds, mains, nil, false
	}
	b, _, err := pb.r.file(pb.r.files[q])
	if err != nil {
		t.Fatal(err)
	}
	lines, _, err := pyTokens(pb.r.ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	conftest, defIn, classIn, impure := path.Base(q) == "conftest.py", -1, -1, false
	for _, l := range lines {
		if defIn >= 0 && l.indent <= defIn {
			defIn = -1
		}
		if classIn >= 0 && l.indent <= classIn {
			classIn = -1
		}
		for _, st := range pySplit(l.toks, ";") {
			if len(st) == 0 || defIn >= 0 || len(st) == 5 && st[0].text == "from" && st[1].text == "." && st[4].text == "*" {
				continue
			} else if pyMain(st) {
				defIn = l.indent
				continue
			}
			names, _ := pyBinds(st)
			main, deco := pyAllowed(st, conftest) != "", false
			if st[0].text == "@" {
				_, _, deco = pyDeco(st)
			}
			if main || st[0].text == "@" && !deco || st[0].text != "@" && !pyStmt(st) || !pyConst(l.inner) || slices.ContainsFunc(names, func(n string) bool {
				return strings.HasPrefix(n, "__") && strings.HasSuffix(n, "__") && n != "__all__" && n != "__slots__" || n == "object"
			}) {
				k, detail := corpusStmtKind(st, l.inner, names)
				impure = true
				add(&kinds, k)
				if detail != "" {
					add(&details, k+"\x00"+detail)
				}
				if main {
					add(&mains, k)
				}
			}
			if d := st[b2i(st[0].text == "async"):]; d[0].text == "def" {
				defIn = l.indent
			} else if d[0].text == "class" && classIn < 0 {
				classIn = l.indent
			}
		}
	}
	if f.impure && !impure {
		add(&kinds, "a pytest decorator whose binding is not proven")
	}
	return kinds, mains, details, impure && !f.impure
}

var corpusCompound = strings.Fields("if elif else try except finally for while with match case")

// corpusStmtKind names the shape of statement st that is not inert, and a
// detail: the callee, decorator, class base or dunder name.
func corpusStmtKind(st, inner []ptok, names []string) (string, string) {
	d, parts := st[b2i(st[0].text == "async"):], pyAssign(st)
	callee := func(t []ptok) string {
		for i := range t {
			if pyCallAt(t, i) {
				return chain(t[:i]) + "(…)"
			}
		}
		return ""
	}
	if i := slices.IndexFunc(names, func(n string) bool {
		return strings.HasPrefix(n, "__") && strings.HasSuffix(n, "__") && n != "__all__" && n != "__slots__"
	}); i >= 0 {
		return "a dunder bound (__init__, __repr__, __eq__, …)", names[i]
	}
	switch {
	case d[0].text == "@":
		end := slices.IndexFunc(d, func(k ptok) bool { return k.text == "(" })
		if end < 0 {
			end = len(d)
		}
		c := pyJoin(d[1:end])
		switch {
		case pyPytest(c, false):
			return "decorator: pytest's, with arguments outside the grammar", c
		case strings.HasSuffix(c, ".setter") || strings.HasSuffix(c, ".getter") || strings.HasSuffix(c, ".deleter"):
			return "decorator: @NAME.setter/getter/deleter", ""
		case slices.Contains(strings.Fields("property staticmethod classmethod abstractmethod abc.abstractmethod"), c):
			return "decorator: @" + c, ""
		case slices.Contains(strings.Fields("dataclass dataclasses.dataclass functools.lru_cache lru_cache functools.cache cache functools.wraps wraps contextmanager contextlib.contextmanager"), c):
			return "decorator: @" + c, ""
		case pyPytest(c, true):
			return "decorator: mock's patch", c
		}
		return "decorator: another", c
	case slices.Contains(corpusCompound, d[0].text):
		return "a compound statement (" + d[0].text + ")", ""
	case d[0].text == "def":
		return "a def header (default or annotation outside the grammar)", ""
	case d[0].text == "class":
		if h := pySplit(d, ":")[0]; len(h) > 2 && pyJoin(h[2:]) != "()" && pyJoin(h[2:]) != "(object)" {
			return "a class with a base or keyword", pyJoin(h[2:min(len(h), 12)])
		}
		return "a class header (other)", ""
	case d[0].text == "import" || d[0].text == "from":
		return "an import outside the allowlist (star, or pytest/fixture rebound)", ""
	case !pyConst(inner):
		return "an f-string field that runs code", ""
	case len(parts) == 1:
		if slices.Contains(strings.Fields("del assert raise global nonlocal return pass"), d[0].text) {
			return "a " + d[0].text + " statement", ""
		} else if c := callee(d); c != "" {
			return "an expression statement with a call", c
		}
		return "an expression statement", ""
	case len(parts) > 2:
		return "a chained assignment", ""
	}
	tg, v := pySplit(parts[0], ":"), parts[1]
	switch {
	case len(tg[0]) > 0 && tg[0][len(tg[0])-1].kind == 'o':
		return "an augmented assignment", ""
	case len(tg[0]) != 1:
		return "an assignment to a target other than one name", ""
	case pyOpaque(v):
		return "value: a comprehension or lambda", ""
	case slices.ContainsFunc(v, func(k ptok) bool {
		return k.kind == 's' && strings.ContainsAny(k.text[:strings.IndexAny(k.text, `"'`)], "fFtT")
	}):
		return "value: an f-string", ""
	case callee(v) != "" && !pyExpr(v):
		return "value: a call", callee(v)
	case pyName(v):
		return "value: a dotted name", ""
	case slices.ContainsFunc(v, func(k ptok) bool { return k.text == "[" }) && !pyExpr(v):
		return "value: a subscript", ""
	case len(tg) == 2 && !pyExpr(tg[1]):
		return "an annotation outside the grammar", ""
	}
	return "value: an operator or other expression", ""
}

// corpusWhy is a finding's or a signal's kind: its text before any detail
// (a statement, a module, a name).
func corpusWhy(why string) string {
	for _, sep := range []string{" (", ": ", " through "} {
		why, _, _ = strings.Cut(why, sep)
	}
	return why
}

// corpusCustom: the configuration file and the setting that make the base's
// pytest configuration custom (pyCustom's checks, in its order), or "".
func corpusCustom(t *testing.T, r *run) string {
	for _, p := range slices.Sorted(maps.Keys(r.files)) {
		if !slices.Contains(pyConfs, path.Base(p)) {
			continue
		}
		b, why, err := r.file(r.files[p])
		if err != nil {
			t.Fatal(err)
		} else if why != "" {
			return p + ": unread (" + why + ")"
		}
		conf := pyConf(p, b, true)
		if i := slices.IndexFunc(pyCollect, func(s string) bool { return strings.Contains(conf, s) }); i >= 0 {
			return p + ": the setting " + strings.TrimSpace(pyCollect[i])
		} else if i := slices.IndexFunc(pyWords.Split(conf, -1), func(w string) bool { return strings.HasPrefix(w, "-") && !pyOpt.MatchString(w) }); i >= 0 {
			return p + ": the option " + pyWords.Split(conf, -1)[i]
		} else if strings.ContainsAny(strings.ReplaceAll(conf, "\r\n", "\n"), "\r\v\f\x1c\x1d\x1e\u0085\u2028\u2029") {
			return p + ": a line break iniconfig and TOML read differently"
		}
		for _, v := range pyPathKey.Split(pyNote.ReplaceAllString(strings.ReplaceAll(conf, "\r\n", "\n"), ""), -1)[1:] {
			if v = pyKey.Split(v, 2)[0]; strings.Contains(v, `\`) || path.Ext(p) != ".toml" && strings.ContainsAny(v, `"'`) {
				return p + ": an escape or a quote in a path key"
			}
			for _, w := range pyWords.Split(strings.TrimLeft(v, " \t=:"), -1) {
				if j := path.Join(path.Dir(p), w); w != "" && (path.IsAbs(w) || j == ".." || strings.HasPrefix(j, "../")) {
					return p + ": a path key outside the root (" + w + ")"
				}
			}
		}
		for _, v := range strings.Split(conf, "addopts")[1:] {
			prev := ""
			for _, w := range pyWords.Split(strings.TrimLeft(pyNote.ReplaceAllString(pyKey.Split(v, 2)[0], ""), " \t=:"), -1) {
				if w != "" && !strings.HasPrefix(w, "-") && !slices.Contains([]string{"--tb", "--color", "--durations", "--capture", "-W"}, prev) {
					return p + ": an addopts word that is no option (" + w + ")"
				}
				prev = w
			}
		}
	}
	return ""
}

// corpusSummary is the report's tables.
func corpusSummary(results []corpusRepoResult, files []corpusFile) string {
	var b strings.Builder
	pct := func(n, of int) string { return fmt.Sprintf("%d (%.1f%%)", n, 100*float64(n)/float64(max(of, 1))) }
	b.WriteString("## Repositories\n\n| Repository | Commit | Files | .py | Judged | Read by the closure | Judged not inert | Read not inert | Judged with a finding | …strict | Base | Why custom | The closure's first unread reason | Guard verdict on a comment line | Seconds |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	var tot corpusRepoResult
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | `%.10s` | %d | %d | %d | %d | %s | %s | %s | %s | %s | %s | %s | %s | %.1f |\n", r.Name, r.Commit, r.Files, r.PyFiles, r.Judged, r.Read,
			pct(r.JudgedNotInert, r.Judged), pct(r.ReadNotInert, r.Read), pct(r.JudgedWithFinds, r.Judged), pct(r.JudgedStrict, r.Judged),
			strings.ReplaceAll(cmp.Or(r.BaseUnread, "read"), "|", "\\|"), cmp.Or(r.Custom, "–"), strings.ReplaceAll(cmp.Or(r.Closure, "read"), "|", "\\|"), strings.ReplaceAll(cmp.Or(strings.Join(r.Verdict, "; "), "passes"), "|", "\\|"), r.Seconds)
		tot.Files, tot.PyFiles, tot.Judged, tot.Read = tot.Files+r.Files, tot.PyFiles+r.PyFiles, tot.Judged+r.Judged, tot.Read+r.Read
		tot.JudgedNotInert, tot.ReadNotInert, tot.JudgedWithFinds, tot.JudgedStrict = tot.JudgedNotInert+r.JudgedNotInert, tot.ReadNotInert+r.ReadNotInert, tot.JudgedWithFinds+r.JudgedWithFinds, tot.JudgedStrict+r.JudgedStrict
	}
	fmt.Fprintf(&b, "| **all** | | %d | %d | %d | %d | %s | %s | %s | %s | | | | | |\n\n", tot.Files, tot.PyFiles, tot.Judged, tot.Read,
		pct(tot.JudgedNotInert, tot.Judged), pct(tot.ReadNotInert, tot.Read), pct(tot.JudgedWithFinds, tot.Judged), pct(tot.JudgedStrict, tot.Judged))
	type count struct{ judged, read, aloneJudged, aloneRead, main int }
	kinds, finds, details, disagree := map[string]*count{}, map[string]int{}, map[string]map[string]int{}, 0
	for _, f := range files {
		disagree += b2i(f.Disagreement)
		for _, k := range f.Kinds {
			if kinds[k] == nil {
				kinds[k] = &count{}
			}
			c := kinds[k]
			c.read, c.judged, c.main = c.read+1, c.judged+b2i(f.Judged), c.main+b2i(slices.Contains(f.MainKinds, k))
			if len(f.Kinds) == 1 {
				c.aloneRead, c.aloneJudged = c.aloneRead+1, c.aloneJudged+b2i(f.Judged)
			}
		}
		for _, k := range f.FindKinds {
			finds[k]++
		}
		for _, kd := range f.Details {
			k, d, _ := strings.Cut(kd, "\x00")
			if details[k] == nil {
				details[k] = map[string]int{}
			}
			details[k][d]++
		}
	}
	b.WriteString("## Why a file is not inert\n\nEach file counts once per kind. \"Alone\": the file's only kind, so relaxing that kind alone would make it inert. \"Main too\": files where main's allowlist (`pyAllowed`) also rejects a statement of that kind.\n\n| Kind | Files read | Judged | Alone (read) | Alone (judged) | Main too |\n|---|---|---|---|---|---|\n")
	for _, k := range slices.SortedFunc(maps.Keys(kinds), func(x, y string) int { return cmp.Or(kinds[y].read-kinds[x].read, strings.Compare(x, y)) }) {
		c := kinds[k]
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %d |\n", strings.ReplaceAll(k, "|", "\\|"), pct(c.read, tot.Read), pct(c.judged, tot.Judged), c.aloneRead, c.aloneJudged, c.main)
	}
	b.WriteString("\n## The most common details, by kind\n\nFiles read by the closure, each once per detail.\n")
	for _, k := range slices.Sorted(maps.Keys(details)) {
		ds := details[k]
		fmt.Fprintf(&b, "\n**%s:** ", k)
		var top []string
		for i, d := range slices.SortedFunc(maps.Keys(ds), func(x, y string) int { return cmp.Or(ds[y]-ds[x], strings.Compare(x, y)) }) {
			if i == 12 {
				break
			}
			top = append(top, fmt.Sprintf("`%s` %d", strings.ReplaceAll(d, "`", "'"), ds[d]))
		}
		b.WriteString(strings.Join(top, ", ") + ".\n")
	}
	b.WriteString("\n## Judged files' import-time findings (Opus #266's measure)\n\n| Finding | Judged files |\n|---|---|\n")
	for _, k := range slices.SortedFunc(maps.Keys(finds), func(x, y string) int { return cmp.Or(finds[y]-finds[x], strings.Compare(x, y)) }) {
		fmt.Fprintf(&b, "| %s | %s |\n", strings.ReplaceAll(k, "|", "\\|"), pct(finds[k], tot.Judged))
	}
	b.WriteString("\n## What widen-b's planned widenings and its report-only kinds cover\n\n")
	b.WriteString("\"Covered\": every kind of the file is one widenings 2–9 of Opus #266 would accept (approximately: by kind, and by callee or dunder name). \"Report-only\": a kind the lead kept out of widen-b's relaxations (#281 rounds 5–8).\n\n| Scope | Not inert | Covered by widenings 2–9 | With a report-only kind | Only report-only kinds |\n|---|---|---|---|---|\n")
	for _, judged := range []bool{true, false} {
		var n, cov, anyF, onlyF int
		for _, f := range files {
			if f.Inert || judged && !f.Judged {
				continue
			}
			n++
			cov += b2i(corpusCovered(f))
			anyF += b2i(slices.ContainsFunc(f.Kinds, func(k string) bool { return slices.Contains(corpusReportOnly, k) }))
			onlyF += b2i(!slices.ContainsFunc(f.Kinds, func(k string) bool { return !slices.Contains(corpusReportOnly, k) }))
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s |\n", map[bool]string{true: "judged files", false: "files read"}[judged], n, pct(cov, n), pct(anyF, n), pct(onlyF, n))
	}
	fmt.Fprintf(&b, "\nClassifier disagreements with `pyBase.file` (a statement the classifier calls impure in a file the guard calls pure): %d.\n", disagree)
	return b.String()
}

// corpusReportOnly are the kinds of #281's blunt rejections the lead kept
// out of widen-b's relaxations (report-only).
var corpusReportOnly = []string{"risky: holds a lambda, a comprehension or a generator at import", "value: a comprehension or lambda", "value: a dotted name",
	"a class with a base or keyword", "risky: names a character by \\N{…}, whose decoding may import unicodedata",
	"risky: has a star import other than from .x import * in a package __init__.py", "risky: imports in a class body or imports a class-private name", "risky: a package star not proved"}

// corpusCovered: every kind of f is one Opus #266's widenings 2–9 would
// accept, by kind, callee or dunder name (an estimate: their side conditions
// are not checked).
func corpusCovered(f corpusFile) bool {
	kinds := strings.Fields("@property @classmethod @staticmethod @NAME.setter/getter/deleter @dataclass @dataclasses.dataclass @functools.lru_cache @lru_cache")
	calls := strings.Fields("Path pathlib.Path os.path.dirname os.path.abspath os.path.realpath os.path.join pytest.importorskip tuple list set frozenset range len zip enumerate reversed sorted min max field logging.getLogger")
	details := map[string][]string{}
	for _, kd := range f.Details {
		k, d, _ := strings.Cut(kd, "\x00")
		details[k] = append(details[k], strings.TrimSuffix(d, "(…)"))
	}
	return !slices.ContainsFunc(f.Kinds, func(k string) bool {
		switch {
		case slices.Contains(kinds, strings.TrimPrefix(k, "decorator: ")), strings.HasPrefix(k, "a compound statement (") && slices.Contains(strings.Fields("if elif else try except finally"), strings.TrimSuffix(strings.TrimPrefix(k, "a compound statement ("), ")")):
			return false
		case k == "value: a call" || k == "an expression statement with a call":
			return len(details[k]) == 0 || slices.ContainsFunc(details[k], func(c string) bool { return !slices.Contains(calls, c) })
		case strings.HasPrefix(k, "a dunder bound"):
			return len(details[k]) == 0 || slices.ContainsFunc(details[k], func(d string) bool {
				return !slices.Contains(strings.Fields("__init__ __repr__ __str__ __post_init__"), d)
			})
		}
		return true
	})
}
