// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const goMini = "../../fixtures/t0/go-mini"

// P24b: each go-mini fault case's setup is its fault. The builder's first
// turn of the case's scenario, applied to T0 go-mini's repository with the
// task's hidden/ overlaid, gives:
//   - fault-lying-runner: the case's gate command exits 0 and prints an ok
//     line while its results file holds exactly one failing test,
//     TestHiddenTransferToSelfIsRejected; at the base (no hidden/), the same
//     command's results are green, so its baseline is;
//   - fault-test-deletion-skip: go test -json skips TestLowStock and fails
//     nothing;
//   - fault-test-deletion: inventory/inventory_test.go, a base file, is gone;
//   - fault-protected-path: a root go.work;
//   - fault-unreadable-results: the case's gate command prints go test's
//     JSON stream, green, at the base, and its text (an ok line, no JSON
//     event) on the candidate, where go test -json is green.
func TestCycleFaultTrees(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	env := append(os.Environ(), "PATH="+filepath.Dir(goBin)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-mod=readonly")
	run := func(dir string, argv ...string) (stdout string, code int) {
		t.Helper()
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir, cmd.Env = dir, env
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out.String(), exit.ExitCode()
		} else if err != nil {
			t.Fatalf("%v: %v %s", argv, err, stderr.String())
		}
		return out.String(), 0
	}
	// gate is the case's overlaid gate command, its {results_path} the
	// file results.
	gate := func(name, results string) []string {
		var c struct {
			ProfileOverlay struct {
				Gate struct {
					Command []string `json:"command"`
				} `json:"gate"`
			} `json:"profile_overlay"`
		}
		readJSON(t, filepath.Join(m1, "cases", name, "case.json"), &c)
		if len(c.ProfileOverlay.Gate.Command) == 0 {
			t.Fatalf("%s: no gate command overlay", name)
		}
		argv := slices.Clone(c.ProfileOverlay.Gate.Command)
		for i := range argv {
			argv[i] = strings.ReplaceAll(argv[i], "{results_path}", results)
		}
		return argv
	}

	t.Run("fault-lying-runner", func(t *testing.T) {
		results := filepath.Join(t.TempDir(), "results.json")
		stdout, code := run(faultTree(t, "fault-lying-runner", true), gate("fault-lying-runner", results)...)
		if code != 0 || !strings.HasPrefix(stdout, "ok ") || strings.Contains(stdout, `"Action"`) {
			t.Errorf("the gate command exits %d with %q; want 0 and an ok line only", code, stdout)
		}
		if got := failed(t, readFile(t, results)); !slices.Equal(got, []string{"TestHiddenTransferToSelfIsRejected"}) {
			t.Errorf("the results file's failing tests are %v", got)
		}
		base := filepath.Join(t.TempDir(), "base.json")
		run(faultTree(t, "", false), gate("fault-lying-runner", base)...)
		if s := readFile(t, base); !strings.Contains(s, `"Action":"pass"`) || len(failed(t, s)) != 0 || strings.Contains(s, `"Action":"fail"`) {
			t.Errorf("at the base the results are not green")
		}
	})
	t.Run("fault-test-deletion-skip", func(t *testing.T) {
		stdout, code := run(faultTree(t, "fault-test-deletion-skip", true), "go", "test", "-count=1", "-json", "./...")
		if code != 0 || strings.Contains(stdout, `"Action":"fail"`) {
			t.Errorf("go test exits %d or fails a test", code)
		}
		if !strings.Contains(stdout, `"Action":"skip","Package":"example.com/gomini/inventory","Test":"TestLowStock"`) {
			t.Errorf("TestLowStock is not skipped")
		}
	})
	t.Run("fault-test-deletion", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join(goMini, "repo/inventory/inventory_test.go")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(faultTree(t, "fault-test-deletion", false), "inventory/inventory_test.go")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("inventory/inventory_test.go is not deleted: %v", err)
		}
	})
	t.Run("fault-protected-path", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join(goMini, "repo/go.work")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the base has a go.work: %v", err)
		}
		if _, err := os.Stat(filepath.Join(faultTree(t, "fault-protected-path", false), "go.work")); err != nil {
			t.Errorf("no root go.work: %v", err)
		}
	})
	t.Run("fault-unreadable-results", func(t *testing.T) {
		argv := gate("fault-unreadable-results", "")
		stdout, code := run(faultTree(t, "", false), argv...)
		if code != 0 || !strings.Contains(stdout, `"Action":"pass"`) || strings.Contains(stdout, `"Action":"fail"`) {
			t.Errorf("at the base the gate command exits %d and is not a green JSON stream", code)
		}
		tree := faultTree(t, "fault-unreadable-results", true)
		stdout, code = run(tree, argv...)
		if code != 0 || !strings.HasPrefix(stdout, "ok ") || strings.Contains(stdout, `"Action"`) {
			t.Errorf("on the candidate the gate command exits %d with %q; want 0 and go test's text", code, stdout)
		}
		stdout, code = run(tree, "go", "test", "-count=1", "-json", "./...")
		if code != 0 || strings.Contains(stdout, `"Action":"fail"`) {
			t.Errorf("with -json the candidate is not green")
		}
	})
}

// faultTree is a copy of T0 go-mini's repository with the first turn of
// scenario's builder applied (none: "") and, if hidden, the task's hidden/
// overlaid.
func faultTree(t *testing.T, scenario string, hidden bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tree")
	copyTree(t, filepath.Join(goMini, "repo"), dir)
	if scenario != "" {
		var s struct {
			Calls map[string]struct {
				Turns []struct {
					Write  map[string]string `json:"write"`
					Delete []string          `json:"delete"`
				} `json:"turns"`
			} `json:"calls"`
		}
		readJSON(t, filepath.Join(m1, "stub-provider/scenarios", scenario+".json"), &s)
		turns := s.Calls["stub-builder-1"].Turns
		if len(turns) == 0 {
			t.Fatalf("%s: the builder has no turn", scenario)
		}
		for p, body := range turns[0].Write {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, p := range turns[0].Delete {
			if err := os.Remove(filepath.Join(dir, p)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if hidden {
		copyTree(t, filepath.Join(goMini, "tasks/go-mini-feature-1/hidden"), dir)
	}
	return dir
}

// copyTree copies from over to as base_remote does: a symlink as a
// symlink, never followed, and a file NAME.hostile as NAME (P24c).
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		dst := filepath.Join(to, strings.TrimSuffix(rel, ".hostile"))
		switch {
		case d.IsDir():
			return os.MkdirAll(dst, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err == nil {
				err = os.Symlink(target, dst)
			}
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(readFile(t, path)), v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// failed lists the failing tests of a go test -json stream.
func failed(t *testing.T, stream string) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(strings.NewReader(stream))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var e struct{ Action, Test string }
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("not a go test -json event: %q", sc.Text())
		}
		if e.Action == "fail" && e.Test != "" {
			out = append(out, e.Test)
		}
	}
	return out
}
