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
	s := suite(t)
	gobin, env := goEnv(t)
	r := Route{Registry: reg, Executor: exe, Secrets: gateway.EnvSecrets{},
		Adapter: session.Config{Command: []string{node, filepath.Join(adapters, "http-chat", "src", "main.ts")},
			Env: append([]string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + t.TempDir()}, testguard.HomeEnv(t)...), Deadline: 15 * time.Minute}}
	ctx := context.Background()
	run := s.NewRun(RouteOf(e), time.Now()) // one id and one seed for every case; the cases run in the seed's order (Astra #306 H1, M5)
	out := map[string]Outcome{}
	for seq, c := range s.Order(run.Seed) {
		p, err := s.Prompt(ctx, c, gobin, t.TempDir(), env)
		if err != nil {
			t.Fatal(err)
		}
		o, err := r.Review(ctx, run, seq, p)
		if err != nil {
			t.Fatalf("%s: %v", c.ID, err)
		}
		out[c.ID] = o
		t.Logf("%d %s: expect %s, decided %q", seq, c.ID, c.Expect, o.Decision)
	}
	dir := os.Getenv("ADC_QUALIFY_OUT")
	if dir == "" {
		dir = filepath.Join(s.Root, Dir, "records")
	}
	name := run.At.Format("2006-01-02") + "-" + strings.ReplaceAll(string(e.ID), "_", "-") + "-" + run.ID[:8]
	rec, err := s.Record(dir, name, run, out)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %d of %d defects no_go, %d go; qualified %v", name, rec.NoGoOnDefects, s.Pass.NoGo, rec.GoOnControls, rec.Qualified)
	if err := Check(dir, RouteOf(e), s); err != nil {
		t.Error(err)
	}
}
