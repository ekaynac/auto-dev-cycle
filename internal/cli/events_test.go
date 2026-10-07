// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

func init() { resumeWait = 0 }

// streamer is a reader whose streams answer one per Follow call, in order;
// past them, a stream ends at once.
type streamer struct {
	reader
	mu      sync.Mutex
	streams []stream
	afters  []int64
}

func (r *streamer) called() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.afters) > 0
}

type stream struct {
	body string
	err  error
}

func (r *streamer) Follow(_ context.Context, after int64) (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.afters = append(r.afters, after)
	if len(r.afters) > len(r.streams) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	s := r.streams[len(r.afters)-1]
	if s.err != nil {
		return nil, s.err
	}
	return io.NopCloser(strings.NewReader(s.body)), nil
}

func data(seq int64, extra string) string {
	return fmt.Sprintf(`{"seq":%d,"id":"evt_%d","at":"t","kind":"grant.created/v1","subject":"grt_1","project":null,"actor":"act_1",`+
		`"command":null,"causation":null,"schema_set":"s","payload":{"grant":{}},"prev_hash":"h","payload_withheld":false%s}`, seq, seq, extra)
}

func frame(seq int64) string {
	return fmt.Sprintf("id: %d\nevent: event\ndata: %s\n\n", seq, data(seq, ""))
}

func closed(o, reason string) string {
	return `event: closed` + "\n" + `data: {"outcome":"` + o + `","reason":"` + reason + `","detail":"gone"}` + "\n\n"
}

const revokedFrame = "event: closed\ndata: {\"outcome\":\"failed\",\"reason\":\"authority_revoked\",\"detail\":\"gone\"}\n\n"

// Events print one per line, byte for byte, from --after; a stream that
// ends without a closed frame resumes after the last seq; a closed frame
// ends it with its result's code and is never resumed.
func TestEventsFollow(t *testing.T) {
	r := &streamer{streams: []stream{{body: frame(6) + frame(9)}, {body: frame(10) + revokedFrame}}}
	code, out, errb := run2(r, "events", "--follow", "--json", "--after", "5")
	if code != entry.ExitRefused || out != data(6, "")+"\n"+data(9, "")+"\n"+data(10, "")+"\n" ||
		fmt.Sprint(r.afters) != "[5 9]" || !strings.Contains(errb, "failed/authority_revoked") {
		t.Fatalf("%d %v\n%s%s", code, r.afters, out, errb)
	}
	r = &streamer{streams: []stream{{body: closed("could_not_run", "store_unreadable")}}}
	if code, _, _ := run2(r, "events", "--follow"); code != entry.ExitCouldNotRun || len(r.afters) != 1 {
		t.Fatalf("store unreadable: %d %v", code, r.afters)
	}
	r = &streamer{streams: []stream{{body: strings.Replace(revokedFrame, "gone", `gone\u001b[2J`, 1)}}}
	if code, _, errb := run2(r, "events", "--follow"); code != entry.ExitRefused || strings.Contains(errb, "\x1b") {
		t.Fatalf("closed detail: %d %q", code, errb)
	}
	// A failed write ends the stream; it is not resumed.
	r = &streamer{streams: []stream{{body: frame(1)}}}
	if code := Main(context.Background(), []string{"events", "--follow"}, failingWriter{"broken pipe"}, io.Discard, r); code != entry.ExitCouldNotRun || len(r.afters) != 1 {
		t.Fatalf("failed write: %d %v", code, r.afters)
	}
	// Human output escapes what the controller sent.
	r = &streamer{streams: []stream{{body: strings.Replace(frame(1), "grt_1", "grt\u202e1", 1) + revokedFrame}}}
	if _, out, _ := run2(r, "events", "--follow"); out != "1 grant.created/v1 grt\\u{202E}1 by act_1 in -\n" {
		t.Fatalf("human: %q", out)
	}
}

// Resuming is bounded: maxResumes ends in a row with no new event stop the
// stream, while a resume that brings an event starts the count again.
func TestEventsResumeBounded(t *testing.T) {
	r := &streamer{}
	if code, _, errb := run2(r, "events", "--follow"); code != entry.ExitCouldNotRun || len(r.afters) != maxResumes+1 || !strings.Contains(errb, "--after 0") {
		t.Fatalf("idle: %d %v %s", code, r.afters, errb)
	}
	var streams []stream
	for i := int64(1); i <= 2*maxResumes; i++ {
		streams = append(streams, stream{body: frame(i)}, stream{})
	}
	r = &streamer{streams: append(streams, stream{body: revokedFrame})}
	if code, _, _ := run2(r, "events", "--follow"); code != entry.ExitRefused || len(r.afters) != len(r.streams) {
		t.Fatalf("progressing: %d %v", code, r.afters)
	}
	// A reconnect that fails is a resume; the first connection is not retried.
	r = &streamer{streams: []stream{{body: frame(1)}, {err: errors.New("dial")}, {body: revokedFrame}}}
	if code, _, _ := run2(r, "events", "--follow"); code != entry.ExitRefused || len(r.afters) != 3 {
		t.Fatalf("failed reconnect: %d %v", code, r.afters)
	}
	r = &streamer{streams: []stream{{err: errors.New("dial")}}}
	if code, _, _ := run2(r, "events", "--follow"); code != entry.ExitCouldNotRun || len(r.afters) != 1 {
		t.Fatalf("first connection: %d %v", code, r.afters)
	}
	// A refusal or a revoked token on reconnect is final.
	for _, err := range []error{client.ErrUnauthenticated, refusal(vocab.Failed, vocab.ReasonActionNotGranted)} {
		r = &streamer{streams: []stream{{body: frame(1)}, {err: err}}}
		if code, _, _ := run2(r, "events", "--follow"); code != entry.ExitRefused || len(r.afters) != 2 {
			t.Fatalf("%v on reconnect: %d %v", err, code, r.afters)
		}
	}
}

// A malformed stream is never resumed: it ends could_not_run at once.
func TestEventsMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"replayed seq":   frame(3) + frame(3),
		"before after":   frame(1),
		"id is not seq":  "id: 4\nevent: event\ndata: " + data(5, "") + "\n\n",
		"unknown field":  "id: 4\nevent: event\ndata: " + data(4, `,"token":"x"`) + "\n\n",
		"no kind":        "id: 4\nevent: event\ndata: " + strings.Replace(data(4, ""), "grant.created/v1", "", 1) + "\n\n",
		"unknown event":  "event: other\ndata: {}\n\n",
		"duplicate line": "id: 4\nid: 4\nevent: event\ndata: " + data(4, "") + "\n\n",
		"unknown line":   "id: 4\nevent: event\nretry: 1\ndata: " + data(4, "") + "\n\n",
		"closed with id": "id: 4\n" + revokedFrame,
		"closed empty":   closed("empty", "completed_clean"),
		"too long":       "id: 2\nevent: event\ndata: " + padded(2, maxFrameBytes) + "\n\n",
	} {
		r := &streamer{streams: []stream{{body: body}}}
		if code, _, _ := run2(r, "events", "--follow", "--after", "1"); code != entry.ExitCouldNotRun || len(r.afters) != 1 {
			t.Errorf("%s: %d %v", name, code, r.afters)
		}
	}
	r := &streamer{streams: []stream{{body: frame(2) + frame(3)}}}
	if code, out, _ := run2(r, "events", "--follow", "--json", "--after", "1"); code != entry.ExitCouldNotRun || len(r.afters) != maxResumes+1 || strings.Count(out, "\n") != 2 {
		t.Errorf("well formed: %d %v %q", code, r.afters, out)
	}
}

func TestEventsUsage(t *testing.T) {
	for _, args := range [][]string{{"events"}, {"events", "--follow", "--bogus"}, {"events", "--follow", "--after", "-1"}, {"events", "--follow", "x"}} {
		r := &streamer{}
		if code, _, _ := run2(r, args...); code != entry.ExitUsage || len(r.afters) != 0 {
			t.Errorf("%v: %d", args, code)
		}
	}
	if code, _, _ := run2(&fake{}, "events", "--follow"); code != entry.ExitCouldNotRun {
		t.Errorf("no transport: %d", code)
	}
}

// A hostile value in every field an event line or a closed frame shows is
// escaped; --json escapes its C1 and DEL controls and keeps the value.
func TestHostileEventFields(t *testing.T) {
	h, _ := json.Marshal(hostile)
	for _, f := range []string{`"kind":"grant.created/v1"`, `"subject":"grt_1"`, `"actor":"act_1"`, `"project":null`} {
		name, _, _ := strings.Cut(f, ":")
		body := "id: 1\nevent: event\ndata: " + strings.Replace(data(1, ""), f, name+":"+string(h), 1) + "\n\n"
		r := &streamer{streams: []stream{{body: body + strings.Replace(revokedFrame, `"gone"`, string(h), 1)}}}
		if code, out, errb := run2(r, "events", "--follow"); code != entry.ExitRefused || raw(out) || raw(errb) ||
			!strings.Contains(out, `\u{202E}`) || !strings.Contains(errb, `\u{202E}`) {
			t.Errorf("%s: %d %q %q", name, code, out, errb)
		}
		r = &streamer{streams: []stream{{body: strings.Replace(body, `\u009b`, "\u009b", 1) + revokedFrame}}}
		if _, out, _ := run2(r, "events", "--follow", "--json"); strings.Contains(out, "\u009b") || !strings.Contains(out, `\u009b`) {
			t.Errorf("%s --json: %q", name, out)
		}
	}
}

// padded is event seq's data with its payload padded so that its line,
// "data: " included, is n bytes.
func padded(seq int64, n int) string {
	d := data(seq, "")
	pad := n - len("data: ") - len(d) - len(`,"pad":""`)
	return strings.Replace(d, `{"grant":{}}`, `{"grant":{},"pad":"`+strings.Repeat("x", pad)+`"}`, 1)
}

// Review cases: a frame cut mid-way, the frame bound, the withheld
// payload, whitespace in an event's data, and cancellation.
func TestEventsEdges(t *testing.T) {
	// A frame cut mid-way is dropped: event 1 is printed and the stream
	// resumes after it.
	r := &streamer{streams: []stream{{body: frame(1) + "id: 2\nevent: event\ndata: {\"seq\":2"}, {body: frame(2) + revokedFrame}}}
	if code, out, _ := run2(r, "events", "--follow", "--json"); code != entry.ExitRefused || fmt.Sprint(r.afters) != "[0 1]" || out != data(1, "")+"\n"+data(2, "")+"\n" {
		t.Errorf("cut: %d %v %q", code, r.afters, out)
	}
	// A line just under the bound passes; one over it is too long.
	line := padded(1, maxFrameBytes-1)
	if len("data: "+line) != maxFrameBytes-1 {
		t.Fatalf("padded to %d", len("data: "+line))
	}
	r = &streamer{streams: []stream{{body: "id: 1\nevent: event\ndata: " + line + "\n\n" + revokedFrame}}}
	if code, out, _ := run2(r, "events", "--follow", "--json"); code != entry.ExitRefused || len(out) != len(line)+1 {
		t.Errorf("under the bound: %d %d", code, len(out))
	}
	r = &streamer{streams: []stream{{body: "id: 1\nevent: event\ndata: " + padded(1, maxFrameBytes+1) + "\n\n" + revokedFrame}}}
	if code, out, errb := run2(r, "events", "--follow"); code != entry.ExitCouldNotRun || out != "" || !strings.Contains(errb, "malformed; resume with --after 0") {
		t.Errorf("over the bound: %d %q", code, errb)
	}
	// The withheld marker.
	withheld := "id: 1\nevent: event\ndata: " + strings.Replace(data(1, ""), `"payload":{"grant":{}},`, `"payload":null,`, 1) + "\n\n"
	withheld = strings.Replace(withheld, `"payload_withheld":false`, `"payload_withheld":true`, 1)
	r = &streamer{streams: []stream{{body: withheld + revokedFrame}}}
	if _, out, _ := run2(r, "events", "--follow"); out != "1 grant.created/v1 grt_1 by act_1 in - (payload withheld)\n" {
		t.Errorf("withheld: %q", out)
	}
	// Tabs between an event's tokens are compacted away.
	r = &streamer{streams: []stream{{body: "id: 1\nevent: event\ndata: " + strings.Replace(data(1, ""), `"seq":1,`, "\t\"seq\" :\t1 ,", 1) + "\n\n" + revokedFrame}}}
	if _, out, _ := run2(r, "events", "--follow", "--json"); out != data(1, "")+"\n" {
		t.Errorf("tabs: %q", out)
	}
	// A closed store_unreadable says where to resume.
	r = &streamer{streams: []stream{{body: frame(3) + closed("could_not_run", "store_unreadable")}}}
	if _, _, errb := run2(r, "events", "--follow", "--after", "2"); !strings.Contains(errb, "resume with --after 3") {
		t.Errorf("unreadable: %q", errb)
	}
	// A cancelled caller is not resumed.
	ctx, cancel := context.WithCancel(context.Background())
	r = &streamer{streams: []stream{{body: frame(1)}}}
	cancelling := &cancelOnFollow{streamer: r, cancel: cancel}
	var out, errb strings.Builder
	if code := Main(ctx, []string{"events", "--follow"}, &out, &errb, cancelling); code != entry.ExitCouldNotRun || len(r.afters) != 1 ||
		!strings.Contains(errb.String(), "cancelled: context canceled; resume with --after 1") {
		t.Errorf("cancelled: %d %v %q", code, r.afters, errb.String())
	}
	// The same while waiting to resume.
	ctx, cancel = context.WithCancel(context.Background())
	resumeWait = time.Hour
	defer func() { resumeWait = 0 }()
	r = &streamer{}
	go func() {
		for !r.called() {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if code := Main(ctx, []string{"events", "--follow"}, &out, &errb, r); code != entry.ExitCouldNotRun || len(r.afters) != 1 {
		t.Errorf("cancelled while waiting: %d %v", code, r.afters)
	}
}

// cancelOnFollow cancels the caller's context once the stream it opens ends.
type cancelOnFollow struct {
	*streamer
	cancel func()
}

func (c *cancelOnFollow) Follow(ctx context.Context, after int64) (io.ReadCloser, error) {
	defer c.cancel()
	return c.streamer.Follow(ctx, after)
}
