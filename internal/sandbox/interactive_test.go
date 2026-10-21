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

// P22a-5d: an interactive run's command is the batch start's (conmon's own
// --timeout included), with --interactive, no detach keys, the cid file in
// the attempt's directory, its exchange (in/ read-only) and the gateway's
// path for the adapter added, and named for its attempt: nothing else
// differs.
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
		"--interactive", "--detach-keys=", "--cidfile="+resolve(r.Attempt.Dir)+"/cid",
		"--mount=type=bind,src="+x+"/in,dst="+ExchangeIn+",ro=true", "--mount=type=bind,src="+x+"/out,dst="+ExchangeOut,
		"--env=ADC_GATEWAY_SOCKET="+GatewaySocket)
	want = append(want, batch[len(batch)-tail:]...)
	if !slices.Equal(it.Command, want) {
		t.Errorf("the interactive command:\n%q\nwant\n%q", it.Command, want)
	}
	if !slices.Contains(it.Command, "--name="+ContainerName(att)) || !slices.Contains(batch, "--timeout=60") || !slices.Equal(it.Env, h.env()) || it.Deadline != r.Limits.Timeout {
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

const testCID = "0d6c1e2b6e7f4a3b2c1d0e9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d2e1f0a9b"

// podmanFake answers rm with rmErr, and ps with listed (and psErr).
func podmanFake(rmErr, psErr error, listed func(filter string) string) *fake {
	return &fake{fn: func(_ context.Context, args []string) ([]byte, error) {
		switch {
		case slices.Contains(args, "rm"):
			return nil, rmErr
		case slices.Contains(args, "ps"):
			for _, a := range args {
				if f, ok := strings.CutPrefix(a, "--filter="); ok && listed != nil {
					return []byte(listed(f)), psErr
				}
			}
			return nil, psErr
		}
		return nil, nil
	}}
}

// Remove removes by the id Podman wrote to the cid file and succeeds only
// once Podman lists that id no more; with no cid file it removes nothing and
// checks the name. Until a call succeeds the attempt is unsafe; each call
// does it all afresh.
func TestInteractiveRemove(t *testing.T) {
	for name, c := range map[string]struct {
		cid     string // "": no cid file
		rm, ps  error
		listed  string
		removed bool
	}{
		"gone":                  {cid: testCID, removed: true},
		"no cid file, gone":     {removed: true},
		"still listed":          {cid: testCID, listed: "adc-x"},
		"no cid file, listed":   {listed: "adc-x"},
		"rm fails":              {cid: testCID, rm: errors.New("rm")},
		"ps fails":              {cid: testCID, ps: errors.New("ps")},
		"a cid file with no id": {cid: "x"},
	} {
		f := podmanFake(c.rm, c.ps, func(string) string { return c.listed })
		h := testHost(t, f)
		att := newAttemptID(t)
		r := testRun(t, h, Build)
		r.Exchange = exchange(t, h, att)
		it, err := h.Interactive(r, att)
		must(t, err)
		if c.cid != "" {
			must(t, os.WriteFile(filepath.Join(r.Attempt.Dir, "cid"), []byte(c.cid+"\n"), 0o600))
		}
		if err := r.Attempt.CopyTree(filepath.Join(t.TempDir(), "before"), 1<<20); !errors.Is(err, ErrSpec) {
			t.Errorf("%s: the tree copied before the removal: %v", name, err)
		}
		err = it.Remove(context.Background())
		if (err == nil) != c.removed {
			t.Errorf("%s: Remove %v", name, err)
		}
		var rms, filters []string
		for _, call := range f.calls {
			if slices.Contains(call, "rm") {
				rms = append(rms, call[len(call)-1])
			}
			for _, a := range call {
				if f, ok := strings.CutPrefix(a, "--filter="); ok {
					filters = append(filters, f)
				}
			}
		}
		switch {
		case c.cid == testCID && (!slices.Equal(rms, []string{testCID}) || c.rm == nil && !slices.Equal(filters, []string{"id=" + testCID})):
			t.Errorf("%s: removed %q, checked %q; want by the id", name, rms, filters)
		case c.cid == "" && (len(rms) != 0 || !slices.Equal(filters, []string{"name=^" + ContainerName(att) + "$"})):
			t.Errorf("%s: removed %q, checked %q; want nothing removed, the name checked", name, rms, filters)
		}
		n := len(f.calls)
		if again := it.Remove(context.Background()); (again == nil) != (err == nil) || len(f.calls) != 2*n {
			t.Errorf("%s: a second Remove: %v, %d podman calls after %d", name, again, len(f.calls), n)
		}
		copyErr := r.Attempt.CopyTree(filepath.Join(t.TempDir(), "after"), 1<<20)
		if (copyErr == nil) != c.removed {
			t.Errorf("%s: the tree after the removal: %v", name, copyErr)
		}
	}
}

// What the run wrote to its exchange's out/ counts against Limits.Disk with
// the attempt's directory: past the bound, Remove fails (#374 M1).
func TestInteractiveRemoveBoundsTheExchange(t *testing.T) {
	h := testHost(t, podmanFake(nil, nil, nil))
	att := newAttemptID(t)
	r := testRun(t, h, Build)
	r.Exchange = exchange(t, h, att)
	it, err := h.Interactive(r, att)
	must(t, err)
	must(t, it.Remove(context.Background()))
	must(t, os.WriteFile(filepath.Join(r.Exchange, "out", "big"), make([]byte, r.Limits.Disk+1), 0o600))
	if err := it.Remove(context.Background()); !errors.Is(err, ErrLeft) {
		t.Fatalf("an out/ past the bound: %v, want ErrLeft", err)
	}
}

// A second run of a live attempt id (its client refused: the name is in use,
// so it wrote no cid file) removes nothing: the first run's container is
// never touched, and the second's Remove fails, since the name is listed
// (#374 M2).
func TestInteractiveRemoveNeverTouchesAnotherRun(t *testing.T) {
	f := podmanFake(nil, nil, func(filter string) string {
		if strings.HasPrefix(filter, "name=") {
			return "the first run's"
		}
		return ""
	})
	h := testHost(t, f)
	att := newAttemptID(t)
	first, second := testRun(t, h, Build), testRun(t, h, Build)
	first.Exchange = exchange(t, h, att)
	second.Exchange = first.Exchange
	_, err := h.Interactive(first, att)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(first.Attempt.Dir, "cid"), []byte(testCID+"\n"), 0o600))
	it, err := h.Interactive(second, att)
	must(t, err)
	if err := it.Remove(context.Background()); err == nil {
		t.Error("the second run's Remove succeeded while the name is listed")
	}
	for _, c := range f.calls {
		if slices.Contains(c, "rm") {
			t.Errorf("a removal: %q", c)
		}
	}
}

// RemoveStale removes every batch run's container and each interactive
// container no runner holds, and only those.
func TestRemoveStale(t *testing.T) {
	held, stale := newAttemptID(t), newAttemptID(t)
	const batch = "adc-0123456789abcdef01234567"
	f := podmanFake(nil, nil, func(filter string) string {
		switch filter {
		case "name=^adc-att_":
			return ContainerName(held) + "\n" + ContainerName(stale) + "\n"
		case "name=^adc-[0-9a-f]{24}$":
			return batch + "\nadc-notours\n"
		}
		return ""
	})
	h := testHost(t, f)
	removed, err := h.RemoveStale(context.Background(), func(a string) bool { return a == held })
	if err != nil || !slices.Equal(removed, []string{stale}) {
		t.Fatalf("removed %q, %v", removed, err)
	}
	var rms []string
	for _, c := range f.calls {
		if slices.Contains(c, "rm") {
			rms = append(rms, c[len(c)-1])
		}
	}
	if !slices.Equal(rms, []string{ContainerName(stale), batch}) {
		t.Errorf("removals: %q", rms)
	}
}

// The probe allows an exchange mount only from the run's own exchange root,
// and none in a run without one, and requires its in/ read-only.
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
	const writable = "the exchange's in/ is writable"
	if v := r.check(); !slices.Contains(v, writable) {
		t.Errorf("an in/ not read-only: %q", v)
	}
	if r.readonly = []string{"/", ExchangeIn}; slices.Contains(r.check(), writable) {
		t.Error("a read-only in/ is reported writable")
	}
}
