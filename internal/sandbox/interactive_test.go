// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

func newAttemptID(t *testing.T) string {
	t.Helper()
	id, err := ids.New("att")
	must(t, err)
	return id
}

// exchange makes attempt's exchange root under the host's exchange root.
func exchange(t *testing.T, h Host, attempt string) string {
	t.Helper()
	x := filepath.Join(h.ExchangeRoot(), "exchange-"+attempt)
	for _, d := range []string{"in", "out"} {
		must(t, os.MkdirAll(filepath.Join(x, d), 0o700))
	}
	return x
}

// P22a-5d: an interactive run's command is the batch start's, with
// --interactive, its exchange (in/ read-only) and the gateway's path for the
// adapter added, and named for its attempt: nothing else differs.
func TestInteractiveIsStartPlusTheExchange(t *testing.T) {
	h := testHost(t, &fake{})
	att := newAttemptID(t)
	r := testRun(t, h, Build)
	batch, err := h.args(r, ContainerName(att))
	must(t, err)
	r.Exchange = exchange(t, h, att)
	it, err := h.Interactive(r, att)
	must(t, err)
	global, err := h.global()
	must(t, err)
	x := resolve(r.Exchange)
	tail := len(r.Argv) + 1 // the image and the command
	want := append(append(append([]string{h.Podman}, global...), batch[:len(batch)-tail]...),
		"--interactive", "--mount=type=bind,src="+x+"/in,dst="+ExchangeIn+",ro=true", "--mount=type=bind,src="+x+"/out,dst="+ExchangeOut,
		"--env=ADC_GATEWAY_SOCKET="+GatewaySocket)
	want = append(want, batch[len(batch)-tail:]...)
	if !slices.Equal(it.Command, want) {
		t.Errorf("the interactive command:\n%q\nwant\n%q", it.Command, want)
	}
	if !slices.Contains(it.Command, "--name="+ContainerName(att)) || !slices.Equal(it.Env, h.env()) || it.Deadline != r.Limits.Timeout {
		t.Errorf("name, environment or deadline: %q %q %s", it.Command, it.Env, it.Deadline)
	}
}

// An interactive run is refused before Podman runs unless it is a build
// run of its attempt's exchange, directly under the exchange root, whose
// in/ and out/ are real directories; a batch start never mounts one.
func TestInteractiveRefuses(t *testing.T) {
	f := &fake{}
	h := testHost(t, f)
	att, other := newAttemptID(t), newAttemptID(t)
	elsewhere := filepath.Join(t.TempDir(), "exchange-"+att) // named right, outside the root
	for _, d := range []string{"in", "out"} {
		must(t, os.MkdirAll(filepath.Join(elsewhere, d), 0o700))
	}
	linked := exchange(t, h, newAttemptID(t))
	must(t, os.Remove(filepath.Join(linked, "in")))
	must(t, os.Symlink(elsewhere, filepath.Join(linked, "in")))
	noOut := exchange(t, h, newAttemptID(t))
	must(t, os.Remove(filepath.Join(noOut, "out")))
	for name, c := range map[string]struct {
		mode     Mode
		exchange string
		attempt  string
	}{
		"a gate run":               {Gate, exchange(t, h, att), att},
		"no exchange":              {Build, "", att},
		"another attempt's":        {Build, exchange(t, h, other), att},
		"not an attempt id":        {Build, exchange(t, h, "x"), "x"},
		"outside the root":         {Build, elsewhere, att},
		"a symlinked in/":          {Build, linked, strings.TrimPrefix(filepath.Base(linked), "exchange-")},
		"no out/":                  {Build, noOut, strings.TrimPrefix(filepath.Base(noOut), "exchange-")},
		"a relative exchange path": {Build, "runs/exchange/exchange-" + att, att},
	} {
		r := testRun(t, h, c.mode)
		r.Exchange = c.exchange
		if _, err := h.Interactive(r, c.attempt); !errors.Is(err, ErrSpec) {
			t.Errorf("%s: %v, want ErrSpec", name, err)
		}
	}
	r := testRun(t, h, Build)
	r.Exchange = exchange(t, h, att)
	if _, err := h.Start(context.Background(), r); !errors.Is(err, ErrSpec) {
		t.Errorf("a batch start with an exchange: %v, want ErrSpec", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("podman ran: %q", f.calls)
	}
}

// Remove succeeds only once Podman lists the container no more; until then
// the attempt is unsafe, and its result is the first call's.
func TestInteractiveRemove(t *testing.T) {
	for name, c := range map[string]struct {
		rm, ps  error
		listed  string
		removed bool
	}{
		"gone":         {removed: true},
		"still listed": {listed: "adc-x\n"},
		"rm fails":     {rm: errors.New("rm")},
		"ps fails":     {ps: errors.New("ps")},
	} {
		f := &fake{fn: func(_ context.Context, args []string) ([]byte, error) {
			switch {
			case slices.Contains(args, "rm"):
				return nil, c.rm
			case slices.Contains(args, "ps"):
				return []byte(c.listed), c.ps
			}
			return nil, nil
		}}
		h := testHost(t, f)
		att := newAttemptID(t)
		r := testRun(t, h, Build)
		r.Exchange = exchange(t, h, att)
		it, err := h.Interactive(r, att)
		must(t, err)
		if err := r.Attempt.CopyTree(filepath.Join(t.TempDir(), "before"), 1<<20); !errors.Is(err, ErrSpec) {
			t.Errorf("%s: the tree copied before the removal: %v", name, err)
		}
		err = it.Remove(context.Background())
		if (err == nil) != c.removed {
			t.Errorf("%s: Remove %v", name, err)
		}
		if again := it.Remove(context.Background()); (again == nil) != (err == nil) || len(f.calls) > 2 {
			t.Errorf("%s: a second Remove: %v, %d podman calls", name, again, len(f.calls))
		}
		if !slices.Contains(f.calls[0], "--time=0") || !slices.Contains(f.calls[0], ContainerName(att)) {
			t.Errorf("%s: the removal: %q", name, f.calls[0])
		}
		if c.rm == nil && !slices.Contains(f.calls[1], "--filter=name=^"+ContainerName(att)+"$") {
			t.Errorf("%s: the check: %q", name, f.calls[1])
		}
		copyErr := r.Attempt.CopyTree(filepath.Join(t.TempDir(), "after"), 1<<20)
		if (copyErr == nil) != c.removed {
			t.Errorf("%s: the tree after the removal: %v", name, copyErr)
		}
	}
}

// RemoveStale removes each interactive container no runner holds, and only
// those: a batch run's and a held attempt's are kept.
func TestRemoveStale(t *testing.T) {
	held, stale := newAttemptID(t), newAttemptID(t)
	var listed bool
	f := &fake{fn: func(_ context.Context, args []string) ([]byte, error) {
		if slices.Contains(args, "ps") && !listed {
			listed = true
			return []byte(ContainerName(held) + "\n" + ContainerName(stale) + "\nadc-0123456789abcdef01234567\n"), nil
		}
		return nil, nil
	}}
	h := testHost(t, f)
	removed, err := h.RemoveStale(context.Background(), func(a string) bool { return a == held })
	if err != nil || !slices.Equal(removed, []string{stale}) {
		t.Fatalf("removed %q, %v", removed, err)
	}
	var rms [][]string
	for _, c := range f.calls {
		if slices.Contains(c, "rm") {
			rms = append(rms, c)
		}
	}
	if len(rms) != 1 || !slices.Contains(rms[0], ContainerName(stale)) {
		t.Errorf("removals: %q", rms)
	}
	if !slices.Contains(f.calls[0], "--filter=name=^adc-att_") {
		t.Errorf("the listing: %q", f.calls[0])
	}
}

// The probe allows an exchange mount only from the run's own exchange root,
// and none in a run without one.
func TestProbeAllowsOnlyItsExchange(t *testing.T) {
	r := Report{Mode: Build, exchange: "exchange-att_a"}
	for _, c := range []struct {
		m    Mount
		want bool
	}{
		{Mount{ExchangeIn, "/r/runs/exchange/exchange-att_a/in", "ext4"}, true},
		{Mount{ExchangeOut, "/r/runs/exchange/exchange-att_a/out", "ext4"}, true},
		{Mount{ExchangeIn, "/r/runs/exchange/exchange-att_b/in", "ext4"}, false},
		{Mount{ExchangeOut, "/r/runs/exchange/exchange-att_a/in", "ext4"}, false},
	} {
		if got := r.allowed(c.m); got != c.want {
			t.Errorf("%+v: allowed %v", c.m, got)
		}
	}
	if (Report{Mode: Build}).allowed(Mount{ExchangeIn, "/r/runs/exchange/exchange-att_a/in", "ext4"}) {
		t.Error("a run with no exchange allowed one")
	}
}
