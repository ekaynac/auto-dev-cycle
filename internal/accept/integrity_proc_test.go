// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// TestProcFrames (H1, Astra #287): a child's message is refused before
// the controller allocates for it: Astra's 32<<20 empty entries, a count
// past what is left of the frame or maxResultEntries, a length past its
// frame, a frame past maxFrame or the run's maxProcOut; and an honest one
// reads back whole.
func TestProcFrames(t *testing.T) {
	emptyLists := []byte{'d', 0, 0} // two lists, no entry
	if m, err := readFrame(bytes.NewReader(append(binary.BigEndian.AppendUint32(nil, uint32(len(emptyLists)+2)), append(emptyLists, 0, 0)...)), new(int64(maxProcOut)), new([][]byte)); err != nil || m.erred {
		t.Fatalf("an empty result: %+v %v", m, err)
	}
	frame := func(body []byte) []byte {
		return append(binary.BigEndian.AppendUint32(nil, uint32(len(body))), body...)
	}
	uv := func(b []byte, n uint64) []byte { return binary.AppendUvarint(b, n) }
	for name, c := range map[string]struct {
		in    []byte
		left  int64
		alloc uint64 // bytes the read may allocate
	}{
		"32<<20 empty entries":                        {frame(uv([]byte{'d'}, 32<<20)), maxProcOut, 1 << 20},
		"maxResultEntries entries, no bytes for them": {frame(uv([]byte{'d'}, uint64(maxResultEntries))), maxProcOut, 1 << 20},
		"maxResultEntries+1 entries":                  {frame(append(uv([]byte{'d'}, uint64(maxResultEntries)+1), make([]byte, 4*(maxResultEntries+1))...)), maxProcOut, 4 << 20},
		"maxResultEntries+1 paths":                    {frame(append(uv([]byte{'l', 0, 0, 0, 0}, uint64(maxResultEntries)+1), make([]byte, maxResultEntries+1)...)), maxProcOut, 4 << 20},
		"paths, no bytes for them":                    {frame(uv([]byte{'l', 0, 0, 0, 0}, 1000)), maxProcOut, 1 << 20},
		"a length past its frame":                     {frame(uv([]byte{'r'}, 1<<30)), maxProcOut, 1 << 20},
		"a frame past maxFrame":                       {binary.BigEndian.AppendUint32(nil, uint32(maxFrame)+1), maxProcOut, 1 << 20},
		"a frame past the run's bound":                {frame(uv(uv([]byte{'r'}, 0), 0)), 4, 1 << 20},
		"an unknown op":                               {frame([]byte{'x'}), maxProcOut, 1 << 20},
		"a field left over":                           {frame(uv(uv(uv([]byte{'r'}, 0), 0), 0)), maxProcOut, 1 << 20},
		"a reason not registered":                     {frame(append(uv(uv(uv(uv(uv(uv([]byte{'d'}, 1), 1), 'x'), 0), 0), 0), 0, 0, 0)), maxProcOut, 1 << 20},
		// #6 (Astra #287 r2): a flag past 1, and an error's text with no error.
		"an error flag of 2 (Astra's frame)": {frame(append(emptyLists, 0, 2)), maxProcOut, 1 << 20},
		"an error's text, its flag false":    {frame(append(emptyLists, 1, 'e', 0)), maxProcOut, 1 << 20},
		"a go.mod presence flag of 3":        {frame([]byte{'l', 0, 0, 3, 0, 0}), maxProcOut, 1 << 20},
		// #2 (Astra #293 r2): a reference to a payload no listing sent.
		"a reference to no payload": {frame([]byte{'l', 0, 0, 2, 0, 0, 0}), maxProcOut, 1 << 20},
	} {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		_, err := readFrame(bytes.NewReader(c.in), &c.left, new([][]byte))
		runtime.ReadMemStats(&after)
		if err == nil || after.TotalAlloc-before.TotalAlloc > c.alloc+uint64(len(c.in)) {
			t.Errorf("%s: %v, %d bytes allocated for %d read", name, err, after.TotalAlloc-before.TotalAlloc, len(c.in))
		}
	}
	honest := childMsg{op: 'd', blocks: []Block{{vocab.ReasonTestIntegrityViolation, "p", strings.Repeat("€", 5000), false, true}}, err: strings.Repeat("e", 9000), erred: true} // whole, however long
	left := maxProcOut
	if m, err := readFrame(bytes.NewReader(honest.frame()), &left, new([][]byte)); err != nil || m.blocks[0] != honest.blocks[0] || m.err != honest.err || !m.erred || len(m.warnings) != 0 {
		t.Errorf("an honest result: %+v %v", m, err)
	}
	// #2 (Astra #287 r2): an absent go.mod or go.sum reads back absent, a
	// present one, empty or not, present.
	for name, c := range map[string][2][]byte{"absent": {nil, nil}, "present and empty": {{}, {}}, "present": {[]byte("module m\n"), []byte("h1\n")}} {
		m, err := readFrame(bytes.NewReader(childMsg{op: 'l', platform: [2]string{"linux", "amd64"}, gomod: c[0], gosum: c[1]}.frame()), &left, new([][]byte))
		if err != nil || (m.gomod == nil) != (c[0] == nil) || (m.gosum == nil) != (c[1] == nil) || string(m.gomod) != string(c[0]) || string(m.gosum) != string(c[1]) {
			t.Errorf("a listing, its go.mod %s: %q %q %v", name, m.gomod, m.gosum, err)
		}
	}
	// #2 (Astra #293 r2): a payload sent once, then referred to; an empty
	// one stays present.
	payloads := [][]byte{}
	first, err := readFrame(bytes.NewReader(childMsg{op: 'l', gomod: []byte("module m\n"), gosum: []byte{}}.frame()), &left, &payloads)
	again, err2 := readFrame(bytes.NewReader(childMsg{op: 'l', gomodRef: 1, gosumRef: 2}.frame()), &left, &payloads)
	if err != nil || err2 != nil || string(again.gomod) != string(first.gomod) || again.gosum == nil || len(again.gosum) != 0 || len(payloads) != 2 {
		t.Errorf("a payload referred to: %q %q, %d payloads, %v %v", again.gomod, again.gosum, len(payloads), err, err2)
	}
}

// fakeChild runs converse against a child played by play over pipes.
func fakeChild(req procMsg, read Reader, list Lister, play func(next func(childMsg) procMsg)) ([]Block, []Block, error, bool) {
	return fakeChildIn(context.Background(), req, read, list, play)
}

// fakeChildIn is fakeChild until ctx ends.
func fakeChildIn(ctx context.Context, req procMsg, read Reader, list Lister, play func(next func(childMsg) procMsg)) ([]Block, []Block, error, bool) {
	cr, cw := io.Pipe()
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		dec := gob.NewDecoder(cr)
		var first procMsg
		if dec.Decode(&first) != nil {
			return
		}
		play(func(m childMsg) procMsg {
			var rep procMsg
			if _, err := pw.Write(m.frame()); err == nil && m.op != 'd' {
				_ = dec.Decode(&rep)
			}
			return rep
		})
	}()
	defer cr.Close()
	defer pr.Close() // a child still writing ends
	blocks, warnings, err, ok, _ := converse(ctx, cw, pr, req, read, list)
	return blocks, warnings, err, ok
}

// TestProcService (M4, M5, Astra #287): what the controller saw is
// latched, whatever the child's result says (a reader's error, a service's
// panic); a read outside the run's blobs, and a listing past maxListRuns,
// are refused and latched; a read never asks for more than maxFile.
func TestProcService(t *testing.T) {
	const own, other = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	req := procMsg{Files: map[string]string{"a.go": own}, Lister: true}
	var asked []string
	var maxAsked int64
	failing := errors.New("git failed")
	read := func(_ context.Context, blob string, max int64) ([]byte, bool, error) {
		asked, maxAsked = append(asked, blob), max
		return nil, false, failing
	}
	boom := func(context.Context, string, [2]string, []byte, []byte, []string) (map[string]Listed, error) {
		panic("boom")
	}
	clean := childMsg{op: 'd'}
	has := func(bs []Block, what string) bool {
		for _, b := range bs {
			if strings.Contains(b.What, what) {
				return true
			}
		}
		return false
	}
	// A reader's error, then a clean result: the reader's error. A read
	// past maxFile is a malformed message.
	_, _, err, ok := fakeChild(req, read, boom, func(next func(childMsg) procMsg) {
		next(childMsg{op: 'r', blob: own, max: maxFile})
		next(clean)
	})
	if !ok || err != failing || maxAsked != maxFile {
		t.Errorf("a reader's error under a clean result: %v %v, max %d", ok, err, maxAsked)
	}
	if _, _, _, ok := fakeChild(req, read, boom, func(next func(childMsg) procMsg) { next(childMsg{op: 'r', blob: own, max: maxFile + 1}) }); ok {
		t.Errorf("a read past maxFile: accepted")
	}
	// A service's panic, then a clean result: the guard's failure.
	blocks, _, err, ok := fakeChild(req, read, boom, func(next func(childMsg) procMsg) {
		if rep := next(childMsg{op: 'l', platform: [2]string{"linux", "amd64"}}); !rep.Panicked || rep.Panic != "boom" {
			t.Errorf("a list's panic: %+v", rep)
		}
		next(clean)
	})
	if !ok || err != nil || !has(blocks, "unverifiable: the guard failed (boom)") {
		t.Errorf("a panic under a clean result: %v %v %q", ok, err, whats(blocks))
	}
	// An empty panic, then a clean result (Opus #339 r2 M1′): latched all
	// the same, its block "the guard failed ()".
	silent := func(context.Context, string, [2]string, []byte, []byte, []string) (map[string]Listed, error) {
		panic("")
	}
	blocks, _, err, ok = fakeChild(req, read, silent, func(next func(childMsg) procMsg) {
		next(childMsg{op: 'l', platform: [2]string{"linux", "amd64"}})
		next(clean)
	})
	if !ok || err != nil || len(blocks) != 1 || blocks[0].What != "unverifiable: the guard failed ()" {
		t.Errorf("an empty panic under a clean result: %v %v %q", ok, err, whats(blocks))
	}
	// A blob outside the run: never read, refused and latched.
	asked = nil
	blocks, _, err, ok = fakeChild(req, read, boom, func(next func(childMsg) procMsg) {
		next(childMsg{op: 'r', blob: other})
		next(clean)
	})
	if !ok || err != nil || len(asked) != 0 || !has(blocks, "asked for a blob outside its run") {
		t.Errorf("a blob outside the run: %v %v %q, read %q", ok, err, whats(blocks), asked)
	}
	// Listings past maxListRuns: refused and latched.
	lists := 0
	count := func(context.Context, string, [2]string, []byte, []byte, []string) (map[string]Listed, error) {
		lists++
		return nil, nil
	}
	blocks, _, _, ok = fakeChild(req, read, count, func(next func(childMsg) procMsg) {
		for range maxListRuns + 1 {
			next(childMsg{op: 'l', platform: [2]string{"linux", "amd64"}})
		}
		next(clean)
	})
	if !ok || lists != maxListRuns || !has(blocks, "past its go list bound") {
		t.Errorf("listings past the bound: %v, %d lists, %q", ok, lists, whats(blocks))
	}
	// #5 (Astra #287 r2): a listing's failure, then a clean result: blocked.
	failed := func(context.Context, string, [2]string, []byte, []byte, []string) (map[string]Listed, error) {
		return nil, errors.New("copy-out failed")
	}
	blocks, _, err, ok = fakeChild(req, read, failed, func(next func(childMsg) procMsg) {
		next(childMsg{op: 'l', platform: [2]string{"linux", "amd64"}})
		next(clean)
	})
	if !ok || err != nil || !has(blocks, "unverifiable: go list failed (copy-out failed)") {
		t.Errorf("a listing's failure under a clean result: %v %v %q", ok, err, whats(blocks))
	}
}

// TestProcServiceSlots (#1, Astra #287 r2; Opus #293 M2, L1, L3):
// services stuck past their runs' deadlines keep their slots until their
// calls really return; with every slot held, a later run waits until its
// own deadline and is never served, so repeated runs leave at most the
// slots' count of stuck calls; once the calls return, runs are served
// again, each slot freed before its reply; and more healthy runs than
// slots all pass, waiting their turn. The 200 ms deadline is only the
// stuck runs'.
func TestProcServiceSlots(t *testing.T) {
	old := serviceSlots
	serviceSlots = make(chan struct{}, 2)
	t.Cleanup(func() { serviceSlots = old })
	const own = "1111111111111111111111111111111111111111"
	req := procMsg{Files: map[string]string{"a.go": own}}
	var entered atomic.Int32
	release := make(chan struct{})
	stuck := func(context.Context, string, int64) ([]byte, bool, error) { // its context ignored
		entered.Add(1)
		<-release
		return nil, false, nil
	}
	run := func(ctx context.Context, read Reader) ([]Block, bool) {
		blocks, _, _, ok := fakeChildIn(ctx, req, read, nil, func(next func(childMsg) procMsg) {
			next(childMsg{op: 'r', blob: own, max: 1})
			next(childMsg{op: 'd'})
		})
		return blocks, ok
	}
	deadline := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		t.Cleanup(cancel)
		return ctx
	}
	for i := range 2 {
		if _, ok := run(deadline(), stuck); ok {
			t.Errorf("run %d: a stuck read gave a result", i)
		}
	}
	for i := range 3 {
		if _, ok := run(deadline(), stuck); ok {
			t.Errorf("run %d with every slot held: a result", i+2)
		}
	}
	if n := entered.Load(); n != 2 || len(serviceSlots) != 2 {
		t.Errorf("stuck calls left behind: %d, slots held %d", n, len(serviceSlots))
	}
	close(release)
	for end := time.Now().Add(10 * time.Second); len(serviceSlots) > 0 && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	served := func(context.Context, string, int64) ([]byte, bool, error) { return []byte("x"), true, nil }
	if blocks, ok := run(context.Background(), served); !ok || len(blocks) != 0 || len(serviceSlots) != 0 {
		t.Errorf("once the calls returned: %v %q, %d slots held", ok, whats(blocks), len(serviceSlots))
	}
	slow := func(context.Context, string, int64) ([]byte, bool, error) {
		time.Sleep(100 * time.Millisecond)
		return []byte("x"), true, nil
	}
	results := make(chan bool, 3)
	for range 3 {
		go func() { blocks, ok := run(context.Background(), slow); results <- ok && len(blocks) == 0 }()
	}
	for i := range 3 {
		if !<-results {
			t.Errorf("healthy run %d of 3 over 2 slots: refused", i)
		}
	}
}

// TestProcInflight (Opus #293 L2): converse, ending with a call still
// running, hands back a channel closed once that call returns.
func TestProcInflight(t *testing.T) {
	const own = "1111111111111111111111111111111111111111"
	release := make(chan struct{})
	stuck := func(context.Context, string, int64) ([]byte, bool, error) { <-release; return nil, false, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	cr, cw := io.Pipe()
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		var first procMsg
		if gob.NewDecoder(cr).Decode(&first) == nil {
			_, _ = pw.Write(childMsg{op: 'r', blob: own, max: 1}.frame())
		}
	}()
	_, _, _, ok, inflight := converse(ctx, cw, pr, procMsg{Files: map[string]string{"a.go": own}}, stuck, nil)
	cr.Close()
	pr.Close()
	if ok || inflight == nil {
		t.Fatalf("a stuck call: %v, inflight %v", ok, inflight)
	}
	select {
	case <-inflight:
		t.Error("inflight closed while its call runs")
	default:
	}
	close(release)
	select {
	case <-inflight:
	case <-time.After(10 * time.Second):
		t.Error("inflight not closed once its call returned")
	}
}

// TestProcDispatch (Opus #293 L4): a binary that does not dispatch the
// guard's process starts none; a child whose environment names another
// parent exits at once, its guard never run.
func TestProcDispatch(t *testing.T) {
	dispatches.Store(false)
	defer dispatches.Store(true)
	var started bool
	procStarted = func(*os.Process) { started = true }
	defer func() { procStarted = nil }()
	blocks, _, err := isolated(context.Background(), sandbox.Repo{}, "", nil, nil, nil, t.TempDir())
	if !errors.Is(err, ErrGuardRun) || started || len(blocks) != 0 || !strings.Contains(err.Error(), "this binary does not dispatch the guard's process") {
		t.Errorf("no dispatch: %q %v, started %v", whats(blocks), err, started)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, childArg)
	cmd.Env = []string{childEnv + "=1"} // not its parent
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 || len(out) != 0 {
		t.Errorf("a child of another parent: %v, %q", err, out)
	}
}

// sameReport: the two verdicts report alike, byte for byte: the report,
// the result's detail of each list, Accepted() and Reason().
func sameVerdict(t *testing.T, in, out Verdict) bool {
	t.Helper()
	return sameReport(t, in.Report, out.Report) && detail("", in.Blocks) == detail("", out.Blocks) && detail("", in.Warnings) == detail("", out.Warnings) &&
		in.Accepted() == out.Accepted() && in.Reason() == out.Reason()
}

// TestProcDifferential (#226 entry 105): generated guard results, the same
// every run (a seeded generator, a fixed count), each sent by a child played
// over pipes through converse, the controller's own path, latches
// included; the verdict Accept makes of what converse returns reports as
// the in-process guard's, byte for byte. The cases mix list lengths around
// the report's bounds (maxReported, lowered), fields at and past maxField
// and 4,096 bytes with multi-byte runes and invalid UTF-8 across the cuts,
// the three ranks, blocks with warnings and without, an error, and a
// service panic the child reports (once, whole) or does not (the latch adds
// it): main's report and detail by construction, as the lists come back
// whole.
func TestProcDifferential(t *testing.T) {
	oldN := reported
	defer func() { reported = oldN }()
	rng := rand.New(rand.NewPCG(105, 1))
	pieces := []string{"a", "€", "\xff", "\xc3", "\x01", "\"", "\n", "é"}
	field := func() string {
		n := rng.IntN(200)
		switch rng.IntN(5) {
		case 0:
			n = maxField - 2 + rng.IntN(5)
		case 1:
			n = 4096 - 3 + rng.IntN(7)
		}
		var b strings.Builder
		for b.Len() < n {
			if rng.IntN(8) == 0 {
				b.WriteString(pieces[rng.IntN(len(pieces))])
			} else {
				b.WriteByte(byte('a' + rng.IntN(26)))
			}
		}
		return b.String()
	}
	list := func() []Block {
		bs := make([]Block, []int{0, 1, 2, 9, 10, 11, 60}[rng.IntN(7)])
		for i := range bs {
			rank := rng.IntN(3)
			bs[i] = Block{vocab.ReasonTestIntegrityViolation, field(), field(), rank > 0, rank > 1}
		}
		return bs
	}
	const cases = 300
	var latched, reported_, errored int
	for c := range cases {
		reported = []int{1, 10, maxReported}[rng.IntN(3)]
		blocks, warnings := list(), list()
		if rng.IntN(4) == 0 {
			blocks = nil
		}
		var guardErr string
		if rng.IntN(10) == 0 {
			guardErr, errored = field(), errored+1
		}
		panicking := guardErr == "" && rng.IntN(3) == 0 // the child asks for a blob outside its run: the service panics, and latches
		if panicking {
			latched++
		}
		expect := slices.Clone(blocks)
		var why string
		if panicking {
			why = "unverifiable: the guard failed (the guard's process asked for a blob outside its run)"
			if rng.IntN(2) == 0 { // the child's guard reports it, as Integrity's does
				blocks, reported_ = append(blocks, Block{vocab.ReasonTestIntegrityViolation, "", why, false, false}), reported_+1
			}
			if guardErr == "" {
				expect = append(expect, Block{vocab.ReasonTestIntegrityViolation, "", why, false, false})
			}
		}
		req := procMsg{Files: map[string]string{"a.go": "b1"}}
		gb, gw, gerr, ok := fakeChild(req, func(context.Context, string, int64) ([]byte, bool, error) { return nil, true, nil }, nil, func(next func(childMsg) procMsg) {
			if panicking {
				next(childMsg{op: 'r', blob: "elsewhere", max: 1})
			}
			next(childMsg{op: 'd', blocks: blocks, warnings: warnings, err: guardErr, erred: guardErr != ""})
		})
		name := fmt.Sprintf("case %d (reported %d; %d blocks, %d warnings, panic %v, error %v)", c, reported, len(blocks), len(warnings), panicking, guardErr != "")
		if !ok || (gerr != nil) != (guardErr != "") || gerr != nil && gerr.Error() != guardErr {
			t.Errorf("%s: ok %v, error %v", name, ok, gerr)
			continue
		}
		if gerr != nil { // Accept returns the guard's error: no verdict
			continue
		}
		if in, out := (Verdict{Blocks: expect, Warnings: warnings}), (Verdict{Blocks: gb, Warnings: gw}); !sameVerdict(t, in, out) {
			t.Errorf("%s: the verdict through converse reports otherwise", name)
		}
	}
	t.Logf("%d cases: %d with a latched panic (%d of them reported by the child), %d with an error", cases, latched, reported_, errored)
	if latched < cases/5 || reported_ < cases/10 || errored < cases/20 {
		t.Error("the generator no longer reaches the edges it is for")
	}
}

// sameReport: the two reports are byte-identical.
func sameReport(t *testing.T, a, b func() ([]byte, error)) bool {
	t.Helper()
	ra, err := a()
	rb, err2 := b()
	if err = errors.Join(err, err2); err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(ra, rb)
}

// TestListerKeepsPresence (#2, Astra #287 r2): LocalLister's cache keeps an
// absent go.mod apart from an empty one: the fallback module's listing is
// never an empty go.mod's.
func TestListerKeepsPresence(t *testing.T) {
	list, pl := testLister(t), [2]string{runtime.GOOS, runtime.GOARCH}
	if _, err := list(context.Background(), "", pl, nil, nil, []string{"fmt"}); err != nil {
		t.Fatalf("no go.mod: %v", err)
	}
	if _, err := list(context.Background(), "", pl, []byte{}, nil, []string{"fmt"}); err == nil {
		t.Error("an empty go.mod listed as the fallback module")
	}
}

// TestProcWire (Opus #339 r2 L2): the child's calls go one at a time, and
// none once its result is sent: a call after it panics (in an abandoned
// worker, which recovers it), and nothing follows the result frame.
func TestProcWire(t *testing.T) {
	var out bytes.Buffer
	var answers bytes.Buffer
	if err := gob.NewEncoder(&answers).Encode(procMsg{OK: true, B: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	w := &wire{out: &out, dec: gob.NewDecoder(&answers)}
	if rep := w.call(childMsg{op: 'r', blob: "b", max: 1}); !rep.OK || string(rep.B) != "x" {
		t.Fatalf("a call before the result: %+v", rep)
	}
	result := childMsg{op: 'd'}.frame()
	before := out.Len()
	if err := w.result(result); err != nil {
		t.Fatal(err)
	}
	refused := func() (p any) {
		defer func() { p = recover() }()
		w.call(childMsg{op: 'r', blob: "b", max: 1})
		return nil
	}()
	if refused != "the guard's result is already sent" || !bytes.Equal(out.Bytes()[before:], result) {
		t.Errorf("a call after the result: %v; %d bytes after it", refused, out.Len()-before-len(result))
	}
}
