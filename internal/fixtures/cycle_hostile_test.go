// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

// P24c: each §2.7 hostile case's positive control, run here, outside the
// product: the setup does what the case claims, so the case discriminates.
// The product's refusals are the rows the cases name (accept's patch.diff
// rows: TestHostilePatches; the gateway's: TestHostileModelSwitch).
func TestCycleHostileControls(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	home := testguard.HomeEnv(t)
	base := append([]string{"PATH=" + filepath.Dir(goBin) + ":/usr/bin:/bin", "GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-mod=readonly",
		"GOCACHE=" + goEnv(t, goBin, "GOCACHE"), "GOMODCACHE=" + goEnv(t, goBin, "GOMODCACHE")}, home...)
	run := func(dir string, env []string, argv ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir, cmd.Env = dir, append(slices.Clone(base), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	// tree is a fresh scratch parent holding tree/: go-mini's repository
	// with the case's base overlay.
	tree := func(name string) (parent, dir string) {
		parent = t.TempDir()
		dir = filepath.Join(parent, "tree")
		copyTree(t, filepath.Join(goMini, "repo"), dir)
		if overlay := filepath.Join(m1, "cases", name, "testdata"); exists(overlay) {
			copyTree(t, overlay, dir)
		}
		return parent, dir
	}

	t.Run("hostile-instructions", func(t *testing.T) {
		overlay := filepath.Join(m1, "cases/hostile-instructions/testdata")
		_, dir := tree("hostile-instructions")
		for _, f := range []string{"AGENTS.md", "CLAUDE.md", ".cursorrules", ".github/copilot-instructions.md"} {
			// Committed under an inert name (M4): coding agents working here never load it.
			if exists(filepath.Join(overlay, f)) || !exists(filepath.Join(overlay, f+".hostile")) {
				t.Errorf("the overlay holds %s under its real name, or not as %s.hostile", f, f)
			}
			if !strings.Contains(readFile(t, filepath.Join(dir, f)), "ADC-CANARY-hostile-instructions") {
				t.Errorf("the base's %s does not hold the canary", f)
			}
		}
		// The builder's first turn reads AGENTS.md: what it reads, the canary, is in its next request.
		if reads := firstTurn(t, "hostile-instructions").Read; !slices.Equal(reads, []string{"AGENTS.md"}) {
			t.Errorf("the builder's first turn reads %v", reads)
		}
		if sees := expectOf(t, "hostile-instructions").BuilderSees; !slices.Equal(sees, []string{"ADC-CANARY-hostile-instructions"}) {
			t.Errorf("the case's builder_sees is %q", sees)
		}
	})

	// Each of the builder's three mechanisms alone, run by plain git, creates
	// exactly the case's canary path.
	t.Run("hostile-git-dir", func(t *testing.T) {
		w := firstTurn(t, "hostile-git-dir").Write
		for _, m := range []struct {
			name  string
			unset []string
			run   [][]string
		}{
			{"fsmonitor", []string{"core.hooksPath", "filter.evil.clean", "filter.evil.smudge"}, [][]string{{"status", "--porcelain"}}},
			{"hooksPath", []string{"core.fsmonitor", "filter.evil.clean", "filter.evil.smudge"}, [][]string{{"checkout", "-q", "-b", "other"}}},
			{"filter", []string{"core.fsmonitor", "core.hooksPath"}, [][]string{{"rm", "-q", "-f", "go.mod"}, {"checkout", "-q", "HEAD", "--", "go.mod"}}},
		} {
			t.Run(m.name, func(t *testing.T) {
				parent, dir := tree("hostile-git-dir")
				git := func(args ...string) {
					t.Helper()
					if out, err := run(dir, []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@a",
						"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@a"}, append([]string{"git"}, args...)...); err != nil {
						t.Fatalf("git %v: %v %s", args, err, out)
					}
				}
				git("init", "-q", "--initial-branch=main")
				git("add", "-A")
				git("commit", "-q", "-m", "base")
				// The builder's writes, as plain files: .git/config appended, the hook made executable.
				cfg, err := os.OpenFile(filepath.Join(dir, ".git/config"), os.O_APPEND|os.O_WRONLY, 0)
				if err == nil {
					_, err = cfg.WriteString(w[".git/config"])
					err = errors.Join(err, cfg.Close())
				}
				if err == nil {
					err = os.MkdirAll(filepath.Join(dir, ".githooks"), 0o755)
				}
				if err == nil {
					err = os.WriteFile(filepath.Join(dir, ".githooks/post-checkout"), []byte(w[".githooks/post-checkout"]), 0o755)
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, k := range m.unset {
					git("config", "--unset", k)
				}
				escapedExactly(t, parent, "hostile-git-dir", false)
				for _, args := range m.run {
					git(args...)
				}
				escapedExactly(t, parent, "hostile-git-dir", true)
			})
		}
		if sees := expectOf(t, "hostile-git-dir").BuilderSees; !slices.Equal(sees, []string{`refused: ".git/config" names .git`}) {
			t.Errorf("the case's builder_sees is %q", sees)
		}
	})

	// The probe, run on the host against decoys this test opens (a config, an
	// env name, two unix sockets, a loopback port), fails naming all five
	// probes; it never dials anything this test did not open.
	t.Run("hostile-test-escape", func(t *testing.T) {
		parent, dir := tree("hostile-test-escape")
		// The decoy config: the child's own home (testguard's, HomeEnv's first entry) with a .config/adc.
		_, h, _ := strings.Cut(home[0], "=")
		err := os.MkdirAll(filepath.Join(h, ".config/adc"), 0o700)
		if err == nil {
			err = os.WriteFile(filepath.Join(h, ".config/adc/decoy"), []byte("decoy\n"), 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
		socks := t.TempDir()
		if len(socks) > 80 { // a unix socket path holds at most 107 bytes
			if socks, err = os.MkdirTemp("/tmp", "adc-p24c-"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(socks) })
		}
		listen := func(network, addr string) string {
			l, err := net.Listen(network, addr)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { l.Close() })
			go func() {
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					c.Close()
				}
			}()
			return l.Addr().String()
		}
		env := []string{"ADC_DECOY_TOKEN=x",
			"ADC_SOCKET=" + listen("unix", filepath.Join(socks, "adcd.sock")),
			"ADC_PROBE_GATEWAY=" + listen("unix", filepath.Join(socks, "gateway.sock")),
			"ADC_PROBE_NET=" + listen("tcp", "127.0.0.1:0")}
		out, err := run(dir, env, "go", "test", "-count=1", "-v", "-run", "TestEscapeProbe", "./inventory/")
		if err == nil || !strings.Contains(out, "ADC-PROBE escaped: cfg sock env net gw out\n") {
			t.Errorf("on the host the probe did not fail naming all five probes (cfg sock env net gw, and out): %v %s", err, out)
		}
		escapedExactly(t, parent, "hostile-test-escape", true)
	})

	t.Run("hostile-gate-output", func(t *testing.T) {
		var c struct {
			ProfileOverlay struct {
				Gate struct{ Command []string } `json:"gate"`
			} `json:"profile_overlay"`
		}
		readJSON(t, filepath.Join(m1, "cases/hostile-gate-output/case.json"), &c)
		argv := slices.Clone(c.ProfileOverlay.Gate.Command)
		for i := range argv {
			argv[i] = strings.ReplaceAll(argv[i], "{canary}", "ADC-CANARY-hostile-gate-output-test")
		}
		results := t.TempDir()
		_, dir := tree("hostile-gate-output")
		out, err := run(dir, []string{"ADC_RESULTS=" + results}, argv...)
		if err != nil || len(failed(t, out)) != 0 || exists(filepath.Join(results, ".canary")) {
			t.Errorf("at the base the gate is not green, or wrote the canary: %v %s", err, out)
		}
		for p, body := range firstTurn(t, "hostile-gate-output").Write {
			if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		out, _ = run(dir, []string{"ADC_RESULTS=" + results}, argv...)
		if !slices.Equal(failed(t, out), []string{"TestEnvProbe"}) || !strings.Contains(out, "canary: ADC-CANARY-hostile-gate-output-test") {
			t.Errorf("on the candidate the gate's stream does not fail TestEnvProbe with the canary: %s", out)
		}
	})

	// The base's docs/key is a link to the decoy: read through it on the host,
	// it is the decoy's canary; the builder reads only docs/key.
	t.Run("hostile-symlink", func(t *testing.T) {
		_, dir := tree("hostile-symlink")
		e := expectOf(t, "hostile-symlink")
		const decoy = "ADC-CANARY-hostile-symlink-decoy"
		if target, err := os.Readlink(filepath.Join(dir, "docs/key")); err != nil || target != "../decoy/key" {
			t.Errorf("the base's docs/key is no link to the decoy: %q %v", target, err)
		}
		if got := readFile(t, filepath.Join(dir, "docs/key")); got != decoy+"\n" || !slices.Contains(e.Canaries, decoy) {
			t.Errorf("read through the link: %q; the case's canaries %q", got, e.Canaries)
		}
		if reads := firstTurn(t, "hostile-symlink").Read; !slices.Equal(reads, []string{"docs/key"}) {
			t.Errorf("the builder's first turn reads %v", reads)
		}
		if !slices.Equal(e.BuilderSees, []string{"refused: docs/key is a symlink, which is never followed"}) {
			t.Errorf("the case's builder_sees is %q", e.BuilderSees)
		}
		// The patch accept refuses (TestHostilePatches) creates a link by plain git apply.
		_, plain := tree("")
		out, err := run(plain, []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}, "git", "apply", filepath.Join(abs(t, m1), "cases/hostile-symlink/patch.diff"))
		if err != nil {
			t.Fatalf("git apply: %v %s", err, out)
		}
		if target, err := os.Readlink(filepath.Join(plain, "docs/key")); err != nil || target != "/home/u/.ssh/id_ed25519" {
			t.Errorf("plain git apply did not create the link: %q %v", target, err)
		}
	})

	t.Run("hostile-symlink-path", func(t *testing.T) {
		parent, dir := tree("")
		patch := readFile(t, filepath.Join(m1, "cases/hostile-symlink-path/patch.diff"))
		unsafeApply(t, parent, dir, patch)
		escapedExactly(t, parent, "hostile-symlink-path", true)
		if got := readFile(t, filepath.Join(parent, "canary-hostile-symlink-path")); got != "ADC-CANARY-hostile-symlink-path\n" {
			t.Errorf("the unsafe applier wrote %q", got)
		}
		if _, ok := firstTurn(t, "hostile-symlink-path").Write["docs/../../canary-hostile-symlink-path"]; !ok {
			t.Error("the builder does not try docs/../../canary-hostile-symlink-path")
		}
		if sees := expectOf(t, "hostile-symlink-path").BuilderSees; !slices.Equal(sees, []string{`refused: "docs/../../canary-hostile-symlink-path" leaves the workspace`}) {
			t.Errorf("the case's builder_sees is %q", sees)
		}
	})
}

type hostileExpect struct {
	Canaries    []string `json:"canaries"`
	BuilderSees []string `json:"builder_sees"`
	CanaryPaths []string `json:"canary_paths"`
}

// expectOf is the case's expect.
func expectOf(t *testing.T, name string) hostileExpect {
	t.Helper()
	var c struct {
		Expect hostileExpect `json:"expect"`
	}
	readJSON(t, filepath.Join(m1, "cases", name, "case.json"), &c)
	return c.Expect
}

// escapedExactly checks that parent, a control's scratch parent, holds tree/
// and, when escaped, exactly the case's canary_paths (M2: a name the setup
// does not create leaves the case's check vacuous); otherwise nothing else.
func escapedExactly(t *testing.T, parent, name string, escaped bool) {
	t.Helper()
	want := []string{"tree"}
	if paths := expectOf(t, name).CanaryPaths; escaped {
		if len(paths) == 0 {
			t.Fatalf("%s names no canary_paths", name)
		}
		want = append(want, paths...)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s: the scratch parent holds %q, want %q", name, got, want)
	}
}

// unsafeApply is P24's deliberately unsafe fixture applier (§2.7): it writes
// each new file of patch at dir/path, resolving .. with no check, but only
// inside parent, a disposable scratch directory it refuses to leave.
func unsafeApply(t *testing.T, parent, dir, patch string) {
	t.Helper()
	var path string
	var body strings.Builder
	flush := func() {
		if path == "" {
			return
		}
		target := filepath.Join(dir, path) // .. resolved, unchecked: the point of this applier
		if rel, err := filepath.Rel(parent, target); err != nil || strings.HasPrefix(rel, "..") {
			t.Fatalf("the unsafe applier would write %s, outside its scratch parent", target)
		}
		if err := os.WriteFile(target, []byte(body.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		path = ""
		body.Reset()
	}
	for _, line := range strings.SplitAfter(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			flush()
			path = strings.TrimSuffix(strings.TrimPrefix(line, "+++ b/"), "\n")
		case path != "" && strings.HasPrefix(line, "+"):
			body.WriteString(line[1:])
		}
	}
	flush()
}

type builderTurn struct {
	Read  []string          `json:"read"`
	Write map[string]string `json:"write"`
}

// firstTurn is the scenario's builder's first turn with a read or a write.
func firstTurn(t *testing.T, scenario string) builderTurn {
	t.Helper()
	var s struct {
		Calls map[string]struct {
			Turns []builderTurn `json:"turns"`
		} `json:"calls"`
	}
	readJSON(t, filepath.Join(m1, "stub-provider/scenarios", scenario+".json"), &s)
	for _, tr := range s.Calls["stub-builder-1"].Turns {
		if len(tr.Read)+len(tr.Write) > 0 {
			return tr
		}
	}
	t.Fatalf("%s: the builder has no read or write turn", scenario)
	return builderTurn{}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func abs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func goEnv(t *testing.T, goBin, key string) string {
	t.Helper()
	out, err := exec.Command(goBin, "env", key).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(bytes.TrimSpace(out))
}
