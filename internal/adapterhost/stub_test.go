// SPDX-License-Identifier: Apache-2.0

//go:build linux

package adapterhost

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
)

// The test binary doubles as a scripted adapter (stubEnv: its plan) and as
// the stub's late-writing descendant (lateEnv: the file it appends to).
const (
	stubEnv = "ADC_HOST_STUB"
	lateEnv = "ADC_HOST_STUB_LATE"
)

func TestMain(m *testing.M) {
	if p := os.Getenv(lateEnv); p != "" {
		time.Sleep(300 * time.Millisecond)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
		if err == nil {
			_, _ = f.WriteString("late")
			f.Close()
		}
		_ = os.WriteFile(os.Getenv("ADC_HOST_STUB_MARKER"), nil, 0o644)
		os.Exit(0)
	}
	if plan := os.Getenv(stubEnv); plan != "" {
		runStub(plan)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// stubPlan is what the stub adapter does in its run.
type stubPlan struct {
	Files  map[string]string `json:"files,omitempty"`  // written in out/
	Links  map[string]string `json:"links,omitempty"`  // symlinks in out/ to an outside file with this content
	Events []string          `json:"events,omitempty"` // event bodies; the stamp is added
	Result string            `json:"result,omitempty"` // the run's result; empty: never answered
	Late   string            `json:"late,omitempty"`   // at shutdown, a descendant appends to out/<late>
	Marker string            `json:"marker,omitempty"` // then creates this file
	// HeartbeatMs: after the events, heartbeats every 20 ms for this long.
	HeartbeatMs int `json:"heartbeat_ms,omitempty"`
	// Shutdown is the shutdown reply (default a clean outcome).
	Shutdown string `json:"shutdown,omitempty"`
}

const stubDescribe = `{"harness":"stub","harness_version":"1.0.0","adapter_version":"0.1.0","contract_version":"1.0.0",` +
	`"capabilities":["patch"],"session_semantics":"fresh_process","tool_surface":[],` +
	`"telemetry":{"usage":true,"upstream":false,"stream_deltas":false,"per_turn_coverage":false},"host_side":false,"needs_native_login":false}`

const clean = `{"outcome":"empty","reason":"completed_clean","detail":""}`

func runStub(raw string) {
	var plan stubPlan
	if json.Unmarshal([]byte(raw), &plan) != nil {
		os.Exit(4)
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(nil, 16<<20)
	out := bufio.NewWriter(os.Stdout)
	send := func(line string) { out.WriteString(line + "\n"); out.Flush() }
	reply := func(id int64, result string) { send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, id, result)) }
	var dir string
	for in.Scan() {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
			Params struct {
				Exchange struct {
					Out string `json:"out"`
				} `json:"exchange"`
				Attempt      string `json:"attempt"`
				FencingToken int64  `json:"fencing_token"`
				Call         string `json:"call"`
			} `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil {
			os.Exit(4)
		}
		switch req.Method {
		case "describe":
			reply(req.ID, stubDescribe)
		case "prepare":
			dir = req.Params.Exchange.Out
			reply(req.ID, clean)
		case "run":
			for name, content := range plan.Files {
				_ = os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
			}
			for name, content := range plan.Links {
				outside := filepath.Join(os.TempDir(), fmt.Sprintf("stub-outside-%d", os.Getpid()))
				_ = os.WriteFile(outside, []byte(content), 0o644)
				_ = os.Symlink(outside, filepath.Join(dir, name))
			}
			stamp := fmt.Sprintf(`"attempt":%q,"fencing_token":%d,"call":%q`, req.Params.Attempt, req.Params.FencingToken, req.Params.Call)
			for _, body := range plan.Events {
				send(`{"jsonrpc":"2.0","method":"adapter.event","params":{` + body + `,` + stamp + `}}`)
			}
			for end := time.Now().Add(time.Duration(plan.HeartbeatMs) * time.Millisecond); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
				send(`{"jsonrpc":"2.0","method":"adapter.event","params":{"event":"heartbeat",` + stamp + `}}`)
			}
			if plan.Result != "" {
				reply(req.ID, plan.Result)
			}
		case "shutdown":
			if plan.Late != "" {
				self, _ := os.Executable()
				child := exec.Command(self)
				child.Env = []string{lateEnv + "=" + filepath.Join(dir, plan.Late), "ADC_HOST_STUB_MARKER=" + plan.Marker}
				_ = child.Start()
			}
			if plan.Shutdown != "" {
				reply(req.ID, plan.Shutdown)
			} else {
				reply(req.ID, clean)
			}
			os.Exit(0)
		default:
			reply(req.ID, clean)
		}
	}
}

// stub is the adapter config playing plan.
func stub(t *testing.T, plan stubPlan) session.Config {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return session.Config{Command: []string{self}, Env: []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + t.TempDir(), stubEnv + "=" + string(b)},
		Deadline: 30 * time.Second}
}

// handleJSON is a handle of attempt for content under name.
func handleJSON(attempt, name, content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf(`{"attempt":%q,"handle":%q,"digest":"sha256:%s","size":%d}`, attempt, name, hex.EncodeToString(sum[:]), len(content))
}

func usageJSON(in int) string {
	return fmt.Sprintf(`"tokens":{"in":%d,"out":1,"cache_read":0,"cache_write":0},"turns":1,"milliseconds":5,"cost":{"amount_micros":%d,"currency":"USD","basis":"cash","estimate":true}`, in, in+1)
}

// usageEvent is a delta of in tokens.
func usageEvent(in int) string {
	return `"event":"usage","sub_call":"s1","sequence":0,` + usageJSON(in)
}

// resultJSON is a found result of kind with handle h and a final of in tokens.
func resultJSON(kind, h string, in int) string {
	return fmt.Sprintf(`{"outcome":"found","reason":"completed_with_findings","detail":"a stub result","result":{"kind":%q,"handle":%s},"usage_final":{%s}}`, kind, h, usageJSON(in))
}
