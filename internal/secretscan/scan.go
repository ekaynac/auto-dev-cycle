// SPDX-License-Identifier: Apache-2.0

package secretscan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/regularfile"
)

// ErrIncomplete: the search could not look at everything it was given (an
// entry that cannot be read; with LinksInside, a link that leads out of the
// roots). It never names the entry: a path may hold a secret.
var ErrIncomplete = errors.New("the search did not complete")

// Search is one walk's question.
type Search struct {
	// Forms: the content forms; a regular file holding one is a finding.
	// None: no content is read (a name search).
	Forms []string
	// Decode: also decode a file that parses as JSON (or JSON lines), and
	// each JSON string inside it that parses as JSON again, at most 16 deep,
	// searching every string for the forms (a canary nested in requests).
	Decode bool
	// Names: an entry below a root with one of these names is a finding,
	// whatever it is (a file, a directory, a link).
	Names []string
	// Own: regular files whose content is theirs to hold, each exempt at its
	// exact path only. A link that resolves to one, or to a directory
	// holding one (a chain too), is a finding: another readable path to it.
	Own []string
	// LinksInside: a link whose target (resolved, or lexically when it
	// leads nowhere) lies outside every root ends the search incomplete
	// (the run's own directory: a link out of it is unexpected). Without
	// it (a sandbox root: a container's layers hold absolute and dangling
	// links) a link is neither followed nor reported, unless it resolves to
	// an Own file.
	LinksInside bool
	// Depth: entries at most this far below a root (1: a root's own
	// entries); 0 or less, no limit.
	Depth int
}

// decodeLimit: a file larger than this, or with a NUL in its first 8 KiB,
// is searched by its bytes alone.
const decodeLimit = 64 << 20

// Run walks each root in lexical order and returns the findings, each by
// the path the walk reached it at. Every entry is read with Lstat, and no
// link is ever followed (the lead's ruling on #443; #226 entry 206): only
// regular files are read, each at its real path, opened as regularfile
// opens (no link, no FIFO swapped in) and checked to be the file the Lstat
// saw, so whatever a link inside a root points at is read where it is. A
// link is resolved as a path only, for Own and LinksInside. An entry or
// file that cannot be read ends the search with ErrIncomplete, never a
// skip; so does ctx ending. A device, fifo or socket holds no file's bytes
// and is not read. A name search (no Forms) matches each listed name
// before it looks at the entry, and takes an entry below a root that is
// gone by then (another process removed it: a shared /tmp) as gone, never
// as a search that did not complete (Opus #443 r3 B1); a content search
// stays strict. A root is named by its caller and resolved once.
func (s Search) Run(ctx context.Context, roots []string) ([]string, error) {
	w := walker{ctx: ctx, s: s, own: map[string]bool{}}
	for _, f := range s.Forms {
		w.forms = append(w.forms, []byte(f))
		w.longest = max(w.longest, len(f))
	}
	for _, o := range s.Own {
		// The walk reaches files under the roots' resolved paths: OWN is
		// compared there, its parent resolved, itself never.
		o = filepath.Clean(o)
		if dir, err := filepath.EvalSymlinks(filepath.Dir(o)); err == nil {
			o = filepath.Join(dir, filepath.Base(o))
		}
		w.own[o] = true
		real, err := filepath.EvalSymlinks(o)
		if err != nil {
			real = o
		}
		w.ownReal = append(w.ownReal, real)
	}
	var real []string
	for _, r := range roots {
		p, err := filepath.EvalSymlinks(r)
		if err != nil {
			return nil, ErrIncomplete
		}
		real = append(real, p)
	}
	w.roots = real
	for _, r := range real {
		if err := w.entry(r, 0); err != nil {
			return nil, ErrIncomplete
		}
	}
	return w.found, nil
}

type walker struct {
	ctx     context.Context
	s       Search
	forms   [][]byte
	longest int
	own     map[string]bool
	ownReal []string
	roots   []string
	found   []string
}

// within: path is dir or below it.
func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}

// entry: one path, at depth below its root.
func (w *walker) entry(path string, depth int) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if depth > 0 && len(w.s.Names) > 0 {
		for _, n := range w.s.Names {
			if filepath.Base(path) == n {
				w.found = append(w.found, path)
				break
			}
		}
	}
	nameOnly := len(w.forms) == 0
	gone := func(err error) bool { return nameOnly && depth > 0 && errors.Is(err, fs.ErrNotExist) }
	fi, err := os.Lstat(path)
	if gone(err) {
		return nil
	}
	if err != nil {
		return err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		if nameOnly {
			return nil // a name search reads no content
		}
		return w.link(path)
	case fi.IsDir():
		if w.s.Depth > 0 && depth >= w.s.Depth {
			return nil
		}
		names, err := readDirNames(path)
		if gone(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, n := range names {
			if err := w.entry(filepath.Join(path, n), depth+1); err != nil {
				return err
			}
		}
		return nil
	case fi.Mode().IsRegular():
		if nameOnly || w.own[path] {
			return nil
		}
		hit, err := w.holds(path, fi)
		if err != nil {
			return err
		}
		if hit {
			w.found = append(w.found, path)
		}
		return nil
	default:
		return nil
	}
}

// link: a link, resolved as a path and never read through. One that
// resolves to an Own file, or to a directory holding one, is a finding;
// with LinksInside, one that leads out of every root is an error.
func (w *walker) link(path string) error {
	t, err := os.Readlink(path)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(t) {
		t = filepath.Join(filepath.Dir(path), t)
	}
	target := filepath.Clean(t)
	if r, err := filepath.EvalSymlinks(path); err == nil {
		target = r // the end of the chain; one that leads nowhere is judged by its first step
	}
	for _, o := range w.ownReal {
		if within(o, target) {
			w.found = append(w.found, path)
			return nil
		}
	}
	if w.s.LinksInside {
		for _, r := range w.roots {
			if within(target, r) {
				return nil
			}
		}
		return errors.New("a link out of the roots")
	}
	return nil
}

// readDirNames: a directory's entries, sorted, read whole.
func readDirNames(path string) ([]string, error) {
	es, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(es))
	for i, e := range es {
		names[i] = e.Name()
	}
	return names, nil
}

// holds reports whether the regular file at path (fi, its Lstat) holds a
// form: in its bytes, read in chunks that overlap by the longest form; or,
// with Decode, in a JSON string inside it. It opens as regularfile opens
// and checks that it opened fi's file (Opus #443 r3 O1).
func (w *walker) holds(path string, want fs.FileInfo) (bool, error) {
	f, err := os.OpenFile(path, regularfile.Flags, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	fi, err := regularfile.Check(f, want)
	if err != nil {
		return false, err
	}
	if w.s.Decode && fi.Size() <= decodeLimit {
		b, err := io.ReadAll(f)
		if err != nil {
			return false, err
		}
		return w.inBytes(b) || (!bytes.Contains(b[:min(len(b), 8192)], []byte{0}) && w.inJSON(string(b))), nil
	}
	// Each read appended to what is kept of the last (the longest form less
	// one byte), so no form is split between two reads.
	buf := make([]byte, w.longest-1+1<<20)
	keep := 0
	for {
		if err := w.ctx.Err(); err != nil {
			return false, err
		}
		n, err := f.Read(buf[keep:])
		if n > 0 {
			end := keep + n
			if w.inBytes(buf[:end]) {
				return true, nil
			}
			keep = min(end, w.longest-1)
			copy(buf, buf[end-keep:end])
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

func (w *walker) inBytes(b []byte) bool {
	for _, f := range w.forms {
		if bytes.Contains(b, f) {
			return true
		}
	}
	return false
}

func (w *walker) inText(s string) bool {
	for _, f := range w.forms {
		if strings.Contains(s, string(f)) {
			return true
		}
	}
	return false
}

// inJSON: the text, or each of its lines, parsed as JSON, every string
// (keys too) searched, and a string that parses as JSON again decoded and
// searched in turn, at most 16 deep.
func (w *walker) inJSON(text string) bool {
	docs := []string{text}
	if strings.Contains(text, "\n") {
		docs = append(docs, strings.Split(text, "\n")...)
	}
	for _, d := range docs {
		if v, ok := parseJSON(d); ok && w.inValue(v, 0) {
			return true
		}
	}
	return false
}

func parseJSON(s string) (any, bool) {
	t := jsTrim(s)
	if t == "" || !strings.ContainsRune(`[{"`, rune(t[0])) {
		return nil, false
	}
	var v any
	if json.Unmarshal([]byte(t), &v) != nil {
		return nil, false
	}
	return v, true
}

func (w *walker) inValue(v any, depth int) bool {
	switch x := v.(type) {
	case string:
		if w.inText(x) {
			return true
		}
		if depth < 16 {
			if inner, ok := parseJSON(x); ok {
				return w.inValue(inner, depth+1)
			}
		}
	case []any:
		for _, e := range x {
			if w.inValue(e, depth) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if w.inValue(k, depth) || w.inValue(e, depth) { // a key decoded as a value is (the reference rule)
				return true
			}
		}
	}
	return false
}
