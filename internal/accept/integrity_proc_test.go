// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// TestProcFrames (H1, Astra #287): a child's message is refused before
// the controller allocates for it: Astra's 32<<20 empty entries, a count
// past what is left of the frame or past maxMsgEntries, a length past its
// frame, a frame past maxFrame or the run's maxProcOut; and an honest one
// reads back whole.
func TestProcFrames(t *testing.T) {
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
	} {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		_, err := readFrame(bytes.NewReader(c.in), &c.left)
		runtime.ReadMemStats(&after)
		if err == nil || after.TotalAlloc-before.TotalAlloc > c.alloc+uint64(len(c.in)) {
			t.Errorf("%s: %v, %d bytes allocated for %d read", name, err, after.TotalAlloc-before.TotalAlloc, len(c.in))
		}
	}
	honest := childMsg{op: 'd', blocks: []Block{{vocab.ReasonTestIntegrityViolation, "p", "w", false, true}}, err: "e", erred: true}
	left := maxProcOut
	if m, err := readFrame(bytes.NewReader(honest.frame()), &left); err != nil || m.blocks[0] != honest.blocks[0] || m.err != "e" || !m.erred || len(m.warnings) != 0 {
		t.Errorf("an honest result: %+v %v", m, err)
	}
}

// fakeChild runs converse against a child played by play over pipes.
func fakeChild(req procMsg, read Reader, list Lister, play func(next func(childMsg) procMsg)) ([]Block, []Block, error, bool) {
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
	return converse(context.Background(), cw, pr, req, read, list)
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
}
