// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// TestProcFrames (H1, Astra #287): a child's message is refused before
// the controller allocates for it: Astra's 32<<20 empty entries, a count
// past what is left of the frame or past maxMsgEntries, a length past its
// frame, a frame past maxFrame or the run's maxProcOut; and an honest one
// reads back whole.
func TestProcFrames(t *testing.T) {
	emptyLists := append(append([]byte{'d'}, make([]byte, 3)...), make([]byte, 3)...) // two lists: no entry, no entry left out, no field clipped
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
		"32<<20 empty entries":                     {frame(uv([]byte{'d'}, 32<<20)), maxProcOut, 1 << 20},
		"maxMsgEntries entries, no bytes for them": {frame(uv([]byte{'d'}, uint64(maxMsgEntries))), maxProcOut, 1 << 20},
		"maxMsgEntries+1 entries":                  {frame(append(uv([]byte{'d'}, uint64(maxMsgEntries)+1), make([]byte, 4*(maxMsgEntries+1))...)), maxProcOut, 4 << 20},
		"a length past its frame":                  {frame(uv([]byte{'r'}, 1<<30)), maxProcOut, 1 << 20},
		"a frame past maxFrame":                    {binary.BigEndian.AppendUint32(nil, uint32(maxFrame)+1), maxProcOut, 1 << 20},
		"a frame past the run's bound":             {frame(uv(uv([]byte{'r'}, 0), 0)), 4, 1 << 20},
		"an unknown op":                            {frame([]byte{'x'}), maxProcOut, 1 << 20},
		"a field left over":                        {frame(uv(uv(uv([]byte{'r'}, 0), 0), 0)), maxProcOut, 1 << 20},
		"a reason not registered":                  {frame(append(uv(uv(uv(uv(uv(uv([]byte{'d'}, 1), 1), 'x'), 0), 0), 0), 0, 0, 0)), maxProcOut, 1 << 20},
		// #6 (Astra #287 r2): a flag past 1, and an error's text with no error.
		// Astra's frame, with this protocol's left-out counts (zero) per list.
		"an error flag of 2 (Astra's frame)":  {frame(append(emptyLists, 0, 2)), maxProcOut, 1 << 20},
		"an error's text, its flag false":     {frame(append(emptyLists, 1, 'e', 0)), maxProcOut, 1 << 20},
		"left-out entries past maxOmitted":    {frame(append(append(uv([]byte{'d', 0}, uint64(maxOmitted)+1), make([]byte, 1+3)...), 0, 0)), maxProcOut, 1 << 20},
		"more clipped fields than 2 an entry": {frame([]byte{'d', 0, 1, 3, 0, 0, 0, 0, 0}), maxProcOut, 1 << 20},
		"a go.mod presence flag of 3":         {frame([]byte{'l', 0, 0, 3, 0, 0}), maxProcOut, 1 << 20},
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
	honest := childMsg{op: 'd', blocks: []Block{{vocab.ReasonTestIntegrityViolation, "p", "w", false, true}}, err: "e", erred: true}
	left := maxProcOut
	if m, err := readFrame(bytes.NewReader(honest.frame()), &left, new([][]byte)); err != nil || m.blocks[0] != honest.blocks[0] || m.err != "e" || !m.erred || len(m.warnings) != 0 {
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
	blocks, warnings, _, err, ok := fakeChildOmittedIn(ctx, req, read, list, play)
	return blocks, warnings, err, ok
}

// fakeChildOmitted is fakeChild, with the result's entries left out.
func fakeChildOmitted(req procMsg, read Reader, list Lister, play func(next func(childMsg) procMsg)) ([]Block, []Block, Omitted, error, bool) {
	return fakeChildOmittedIn(context.Background(), req, read, list, play)
}

func fakeChildOmittedIn(ctx context.Context, req procMsg, read Reader, list Lister, play func(next func(childMsg) procMsg)) ([]Block, []Block, Omitted, error, bool) {
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
	blocks, warnings, omitted, err, ok, _ := converse(ctx, cw, pr, req, read, list)
	return blocks, warnings, omitted, err, ok
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
	// Astra #311 #2: a result's left-out entries come back as their counts.
	var omitted Omitted
	omitted[1].Entries, omitted[1].Clipped = 1<<30, 3
	_, warnings, back, _, ok := fakeChildOmitted(req, read, boom, func(next func(childMsg) procMsg) {
		next(childMsg{op: 'd', warnings: []Block{{vocab.ReasonTestIntegrityViolation, "p", "w", false, false}}, omitted: omitted})
	})
	if !ok || len(warnings) != 1 || back != omitted {
		t.Errorf("left-out warnings: %v %d %v", ok, len(warnings), back)
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
	_, _, _, _, ok, inflight := converse(ctx, cw, pr, procMsg{Files: map[string]string{"a.go": own}}, stuck, nil)
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
	blocks, _, _, err := isolated(context.Background(), sandbox.Repo{}, "", nil, nil, nil, t.TempDir())
	if err != nil || started || len(blocks) != 1 || blocks[0].What != "unverifiable: this binary does not dispatch the guard's process" {
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

// viaChild is a guard's result as Integrity's process gives it back: the
// prefix through gob as procMsg carries it, the result fit (fitReport) and
// through its frame, read back as converse reads it; the verdict that
// Accept makes of it.
func viaChild(t *testing.T, prefix, blocks, warnings []Block) Verdict {
	t.Helper()
	var buf bytes.Buffer
	var req procMsg
	if err := gob.NewEncoder(&buf).Encode(procMsg{Prefix: prefixOf(prefix)}); err != nil {
		t.Fatal(err)
	} else if err := gob.NewDecoder(&buf).Decode(&req); err != nil {
		t.Fatal(err)
	}
	theirs, err := blocksOf(req.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	kept, omitted := fitReport(theirs, blocks, warnings)
	m, err := readFrame(bytes.NewReader(childMsg{op: 'd', blocks: kept[0], warnings: kept[1], omitted: omitted}.frame()), new(int64(maxProcOut)), new([][]byte))
	if err != nil {
		t.Fatal(err)
	}
	return Verdict{Blocks: m.blocks, Warnings: slices.Concat(prefix, m.warnings), omitted: m.omitted}
}

// sameAsInProcess: the verdict through the child (viaChild) reports as the
// in-process one, byte for byte: its report, its detail, its acceptance
// and reason; and with Accept's own block after the guard's (none).
func sameAsInProcess(t *testing.T, name string, prefix, blocks, warnings []Block) {
	t.Helper()
	in, out := Verdict{Blocks: blocks, Warnings: slices.Concat(prefix, warnings)}, viaChild(t, prefix, blocks, warnings)
	if !sameReport(t, in.Report, out.Report) || in.Accepted() != out.Accepted() || in.Reason() != out.Reason() ||
		detail("", in.Warnings, 0) != detail("", out.Warnings, out.omitted[1].Entries) || detail("", in.Blocks, 0) != detail("", out.Blocks, out.omitted[0].Entries) {
		a, _ := in.Report()
		b, _ := out.Report()
		t.Errorf("%s: the report or detail through the child differs (%d and %d bytes)", name, len(a), len(b))
	}
	if len(blocks) == 0 {
		in.Blocks, out.Blocks = []Block{tooLarge}, []Block{tooLarge}
		if !sameReport(t, in.Report, out.Report) {
			t.Errorf("%s, with Accept's block: the report through the child differs", name)
		}
	}
}

// TestProcFit (Opus #293 M1; Astra #293 r2 #3, #311): a result fit to the
// frame keeps what the report and the detail read, a field cut on a rune's
// start, and counts the rest exactly: the verdict's report is the
// in-process one's, byte for byte, whatever warnings the verdict held
// before the guard's, and with Accept's own block after the guard.
func TestProcFit(t *testing.T) {
	long := strings.Repeat("€", maxResultField) // 3 bytes a rune: maxResultField falls inside one
	var mixed []Block
	for i := range maxReported + 5 {
		mixed = append(mixed, Block{vocab.ReasonTestIntegrityViolation, "p", "w", false, false}, Block{vocab.ReasonTestIntegrityViolation, long, "w" + strconv.Itoa(i), true, true})
	}
	// Astra's input: 1,001 warnings of 1,000-byte paths exhaust the
	// report's bytes before its count; then smaller ones it may take.
	var wide []Block
	for i := range 1001 {
		wide = append(wide, Block{vocab.ReasonTestIntegrityViolation, "tool/many.go: f" + strings.Repeat("a", 966) + fmt.Sprintf("%04d", i), "fmt.Println in f", false, false})
	}
	smaller := append(slices.Clone(wide), Block{vocab.ReasonTestIntegrityViolation, "x.go", strings.Repeat("s", 480), false, false})
	// A warning before the guard's of a size with which the report takes the
	// smaller one at the end, and without which it does not: the guard's
	// process must count it.
	var ignored []Block
	for n := 1; n < 1200 && ignored == nil; n++ {
		p := []Block{{vocab.ReasonCompletedWithFindings, strings.Repeat("o", n), "output .gitignore kept out of the diff", false, false}}
		with, _, _, _, _ := selection([2][]Block{nil, slices.Concat(p, smaller)})
		without, _, _, _, _ := selection([2][]Block{nil, smaller})
		afterBlock, _, _, _, _ := selection([2][]Block{{tooLarge}, smaller})                        // Accept's block shifts the budget too
		if last := len(smaller) - 1; with[1][1+last] && !without[1][last] && !afterBlock[1][last] { // taken only after the prefix
			ignored = p
		}
	}
	if ignored == nil {
		t.Fatal("no prefix changes the report's choice")
	}
	for name, c := range map[string]struct{ prefix, blocks, warnings []Block }{
		"ranks past maxReported":          {nil, nil, mixed},
		"Astra's 1,001 wide warnings":     {nil, nil, wide},
		"a smaller warning after them":    {nil, nil, smaller},
		"warnings already in the verdict": {ignored, nil, smaller},
		"blocks and warnings":             {nil, mixed, wide},
	} {
		sameAsInProcess(t, name, c.prefix, c.blocks, c.warnings)
		kept, _ := fitReport(c.prefix, c.blocks, c.warnings)
		for _, b := range slices.Concat(kept[0], kept[1]) {
			if b.Path == long[:len(b.Path)] && (len(b.Path) > maxResultField || !utf8.ValidString(b.Path) || len(b.Path) < maxResultField-utf8.UTFMax) {
				t.Fatalf("%s: a cut path of %d bytes", name, len(b.Path))
			}
		}
	}
}

// TestProcFitManyOmitted (Astra #311 #2): 1,049,577 short warnings, every
// 1,000th with a field the report clips: the report's truncated (1,048,577)
// and clipped, and the detail's "more", are the in-process guard's, past
// any count of entries the frame could carry.
func TestProcFitManyOmitted(t *testing.T) {
	clip := strings.Repeat("c", maxField+1)
	warnings := make([]Block, 1_049_577)
	for i := range warnings {
		warnings[i] = Block{vocab.ReasonTestIntegrityViolation, "p", "w", false, false}
		if i%1000 == 999 {
			warnings[i].What = clip
		}
	}
	sameAsInProcess(t, "1,049,577 warnings", nil, nil, warnings)
	report, err := viaChild(t, nil, nil, warnings).Report()
	var r struct{ Truncated, Clipped int }
	if err != nil || json.Unmarshal(report, &r) != nil || r.Truncated != 1_048_577 || r.Clipped != 1_049 {
		t.Errorf("through the child: truncated %d, clipped %d, %v; want 1,048,577 and 1,049", r.Truncated, r.Clipped, err)
	}
}

// TestProcFitDifferential (Astra #311): generated guard results, the same
// for every run (a seeded generator, a fixed count), each reported in this
// process and through the child (viaChild), byte for byte. Each case lowers
// the report's bounds (reported, reportedBytes) so that its lists cross
// them, and mixes the edges where a fit can go wrong: list lengths around
// detailKept and the bounds, the three ranks, fields at and past maxField
// and maxResultField (multi-byte runes across the cuts), invalid UTF-8 in
// the prefix and the guard's own entries, a budget ending a byte either
// side of an entry's end, blocks with warnings and without.
func TestProcFitDifferential(t *testing.T) {
	oldN, oldBytes := reported, reportedBytes
	defer func() { reported, reportedBytes = oldN, oldBytes }()
	rng := rand.New(rand.NewPCG(311, 4))
	pieces := []string{"a", "€", "\xff", "\xc3", "\x01", "\"", "\n", "é"}
	field := func() string {
		var n int
		switch rng.IntN(6) {
		case 0:
			n = maxField - 2 + rng.IntN(5)
		case 1:
			n = maxResultField - 3 + rng.IntN(7)
		case 2:
			n = rng.IntN(3)
		default:
			n = rng.IntN(200)
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
	edges := []int{0, 1, 2, detailKept - 1, detailKept, detailKept + 1, 250, 400}
	list := func(reason vocab.Reason, max int) []Block {
		bs := make([]Block, min(edges[rng.IntN(len(edges))], max))
		for i := range bs {
			rank := rng.IntN(3)
			bs[i] = Block{reason, field(), field(), rank > 0, rank > 1}
		}
		return bs
	}
	const cases = 400
	var omitting, invalid, probes, withBlocks int // what the cases reached: the generator's own check
	for c := range cases {
		reported = []int{1, 5, 50, 199, 201, maxReported}[rng.IntN(6)]
		reportedBytes = 500 + rng.IntN(40_000)
		prefix := list(vocab.ReasonCompletedWithFindings, 3)
		for i := range prefix {
			prefix[i].Additive, prefix[i].Typed = false, false
		}
		var blocks []Block
		if rng.IntN(3) == 0 {
			blocks = list(vocab.ReasonTestIntegrityViolation, 400)
		}
		warnings := list(vocab.ReasonTestIntegrityViolation, 400)
		// Two cases in three, the budget ends a byte either side of where an
		// entry the report takes ends: the choice a cost's change flips.
		if rng.IntN(3) > 0 {
			reportedBytes = 1 << 30
			if _, out, _, _, _ := selection([2][]Block{blocks, slices.Concat(prefix, warnings)}); len(out[0])+len(out[1]) > 0 {
				var ends []int
				used := 0
				for _, l := range out {
					for _, b := range l {
						e, _ := json.Marshal(b)
						used += len(e) + 1
						ends = append(ends, used)
					}
				}
				reportedBytes, probes = max(ends[rng.IntN(len(ends))]+rng.IntN(3)-1, 1), probes+1
			} else {
				reportedBytes = 500 + rng.IntN(40_000)
			}
		}
		if _, o := fitReport(prefix, blocks, warnings); o[0].Entries+o[1].Entries > 0 {
			omitting++
		}
		if slices.ContainsFunc(prefix, func(b Block) bool { return !utf8.ValidString(b.Path + b.What) }) {
			invalid++
		}
		withBlocks += b2i(len(blocks) > 0)
		sameAsInProcess(t, fmt.Sprintf("case %d (reported %d, %d bytes; %d prefix, %d blocks, %d warnings)", c, reported, reportedBytes, len(prefix), len(blocks), len(warnings)), prefix, blocks, warnings)
	}
	t.Logf("%d cases: %d leave entries out, %d with an invalid UTF-8 prefix, %d with the budget at an entry's edge, %d with blocks", cases, omitting, invalid, probes, withBlocks)
	if omitting < cases/4 || invalid < cases/10 || probes < cases/10 || withBlocks < cases/10 {
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
