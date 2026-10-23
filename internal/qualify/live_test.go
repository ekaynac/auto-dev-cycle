// SPDX-License-Identifier: Apache-2.0

//go:build qualify && linux

package qualify

// The live qualification run (the lead's decision 4): the suite through a
// registry's reviewer route, against its real provider, writing the record
// to commit. Never part of make ci. Run, on the controller host:
//
//	ADC_QUALIFY_REGISTRY=<registry.json> ADC_QUALIFY_GROUPS=<groups.json> \
//	ADC_QUALIFY_EXECUTOR=<exe_…> ADC_CREDENTIAL_<REF>=… \
//	  go test -tags qualify -run TestQualifyLive -timeout 2h ./internal/qualify/
//
// The credential is read by the gateway (gateway.EnvSecrets) and never
// printed. ADC_QUALIFY_OUT names the records directory (default: the
// suite's records/).

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

func TestQualifyLive(t *testing.T) {
	regPath, groupsPath, exe := os.Getenv("ADC_QUALIFY_REGISTRY"), os.Getenv("ADC_QUALIFY_GROUPS"), types.ExecutorID(os.Getenv("ADC_QUALIFY_EXECUTOR"))
	if regPath == "" || groupsPath == "" || exe == "" {
		t.Skip("set ADC_QUALIFY_REGISTRY, ADC_QUALIFY_GROUPS and ADC_QUALIFY_EXECUTOR")
	}
	doc, err := registry.Load(regPath)
	if err != nil {
		t.Fatal(err)
	}
	var groups []types.ResourceGroup
	raw, err := os.ReadFile(groupsPath)
	if err == nil {
		err = json.Unmarshal(raw, &groups)
	}
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.New(doc, groups)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := reg.Executor(exe)
	if !ok {
		t.Fatalf("%s is not in the registry", exe)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is not on PATH")
	}
	adapters, err := filepath.Abs(filepath.Join(root, "adapters"))
	if err != nil {
		t.Fatal(err)
	}
	// Without the workspace's dependencies the adapter exits before its first
	// frame, which the host reads as adapter_protocol_error on every case.
	if missing := adapterDeps(t, filepath.Join(adapters, "http-chat")); len(missing) > 0 {
		t.Fatalf("the adapters workspace has no dependencies (http-chat lacks %s): run make ts-deps first", strings.Join(missing, ", "))
	}
	s := suite(t)
	gobin, env := goEnv(t)
	r := Route{Registry: reg, Executor: exe, Secrets: gateway.EnvSecrets{},
		Adapter: session.Config{Command: []string{node, filepath.Join(adapters, "http-chat", "src", "main.ts")},
			Env: append([]string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + t.TempDir()}, testguard.HomeEnv(t)...), Deadline: 15 * time.Minute}}
	ctx := context.Background()
	prompts := map[string][]byte{}
	for _, c := range s.Cases { // every prompt before the run begins
		if prompts[c.ID], err = s.Prompt(ctx, c, gobin, t.TempDir(), env); err != nil {
			t.Fatal(err)
		}
	}
	dir := os.Getenv("ADC_QUALIFY_OUT")
	if dir == "" {
		dir = filepath.Join(s.Root, Dir, "records")
	}
	// Begin writes the run's incomplete record before the first review: an
	// aborted run leaves it, and it fails qualification (Opus #306 r2 M2).
	// One id and one seed for every case; the cases run in the seed's
	// order (Astra #306 H1, M5).
	at := time.Now().UTC()
	name := at.Format("2006-01-02T150405Z") + "-" + strings.ReplaceAll(string(e.ID), "_", "-")
	run, err := s.Begin(dir, name, RouteOf(e), at)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: run %s begun; its record stays incomplete unless the run ends", name, run.ID)
	out := map[string]Outcome{}
	for seq, c := range s.Order(run.Seed) {
		o, err := r.Review(ctx, run, c, prompts[c.ID])
		if err != nil {
			t.Fatalf("review %d: %v", seq, err)
		}
		out[c.ID] = o
		t.Logf("review %d of %d done", seq+1, len(s.Cases)) // no case id, expectation or decision until the record is complete
	}
	rec, err := s.Record(run, out)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rec.Cases {
		t.Logf("%d %s: expect %s, decided %s", c.Evidence.Seq, c.ID, c.Expect, c.Decision)
	}
	t.Logf("%s: %d of %d defects no_go, %d go; qualified %v", name, rec.NoGoOnDefects, s.Pass.NoGo, rec.GoOnControls, rec.Qualified)
	// The route qualifies on its latest two runs, of different seeds
	// (#226 entry 110): after one passing run, Check still refuses it.
	t.Logf("Check on %s: %v", dir, Check(dir, RouteOf(e), s))
	if !rec.Qualified {
		t.Error("this run is under the bar: commit its record all the same")
	}
}

// adapterDeps is the dependencies of the package at dir (its package.json's)
// that its node_modules lacks.
func adapterDeps(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatal(err)
	}
	var missing []string
	for name := range pkg.Dependencies {
		if _, err := os.Stat(filepath.Join(dir, "node_modules", name, "package.json")); err != nil {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return missing
}
