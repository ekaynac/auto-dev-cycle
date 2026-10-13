// SPDX-License-Identifier: Apache-2.0

//go:build linux

package adapterhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/ingest"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	leaID = types.LeaseID("lea_01m3250v57fhm9v1z9xd1wk8gw")
	untID = types.UnitID("unt_01m3250vz2ezn81cgvbkcn17jd")
	attID = types.AttemptID("att_01m3250vnfeyrv67x6w0ddqr6a")
	calID = types.CallID("cal_01m3250v9xft08ssr4ygq6g2np")
	actID = "act_01m3250v0aer9ss2097svmefj8"
)

// patch is what the fake's exchange scenarios write (one turn).
const patch = "--- a/file1.go\n+++ b/file1.go\n@@ -1 +1 @@\n-old\n+new\n"

var base = types.AttemptBase{Tree: "git:" + "0123456789abcdef0123456789abcdef01234567", ContractRevision: "7"}

type world struct {
	t    *testing.T
	dir  string // the state directory
	host *Host
}

// newWorld is a store with a granted lease (revision 2) and a host on it.
func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	lock, err := eventstore.AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	kinds := lease.Kinds()
	maps.Copy(kinds, ledger.Kinds())
	project := lease.Projector{Blobs: blobs, Holds: reg.Holds, Units: anyUnit{}}.Project
	s, err := eventstore.Open(ctx, lock, eventstore.Config{Kinds: kinds, Blobs: blobs,
		Hooks: eventstore.Hooks{Identify: func(*eventstore.Tx, eventstore.Command) error { return nil }, Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil },
			Project: func(tx *eventstore.Tx, evs []eventstore.Event) error {
				return errors.Join(project(tx, evs), ledger.Project(tx, evs))
			}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); lock.Release() })
	w := &world{t: t, dir: dir, host: &Host{Store: s, Blobs: blobs, Actor: actID, Root: t.TempDir()}}
	ok := types.PlacementCheck{Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean}
	l := types.Lease{ID: leaID, Unit: untID, Role: "builder", Executor: "exe_01m3250vnfeyrv67x6w0ddqr6a", State: types.LeaseStateRequested,
		Reservations: []types.LeaseReservationsItem{{Group: "rg_01m3250vkwffc979nzjw0g2sf9", Amount: 1000, Unit: types.CapacityUnitTokens}},
		Footprint:    types.LeaseFootprint{ContextTokens: 1000, ExpectedTurns: 5}, Deadline: "2026-10-04T14:00:00Z",
		PlacementChecks: types.LeasePlacementChecks{DataFloor: ok, Destinations: ok, Capability: ok, Independence: ok,
			PaidAuthority: ok, Budget: ok, FallbackScope: ok}}
	w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, l) })
	w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, leaID, 1, lease.LeaseEvent{Kind: lease.Granted}, reg.Holds)
	})
	return w
}

func (w *world) do(apply func(*eventstore.Tx) ([]eventstore.Event, error)) {
	w.t.Helper()
	if _, err := w.host.command(context.Background(), attID, "test", applies(apply)); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) view(fn func(tx *eventstore.Tx) error) {
	w.t.Helper()
	if err := w.host.Store.View(context.Background(), fn); err != nil {
		w.t.Fatal(err)
	}
}

// record reads one lease or attempt record from its projection.
func record[T any](w *world, projection, id string) (T, bool) {
	var s struct {
		Record T `json:"record"`
	}
	var ok bool
	w.view(func(tx *eventstore.Tx) error {
		raw, found, err := tx.Get(projection, id)
		if err != nil || !found {
			return err
		}
		ok = true
		return json.Unmarshal(raw, &s)
	})
	return s.Record, ok
}

func (w *world) untrustworthy() bool {
	var u bool
	w.view(func(tx *eventstore.Tx) (err error) { u, err = lease.Untrustworthy(tx, attID); return err })
	return u
}

// usage is the call's recorded measurements of this attempt.
func (w *world) usage() []types.UsageMeasurement {
	var ms []types.UsageMeasurement
	w.view(func(tx *eventstore.Tx) (err error) { ms, err = ledger.Measurements(tx, calID); return err })
	return ms
}

func (w *world) published(content string) bool {
	sum := sha256.Sum256([]byte(content))
	has, err := w.host.Blobs.Has("sha256:" + hex.EncodeToString(sum[:]))
	if err != nil {
		w.t.Fatal(err)
	}
	return has
}

func (w *world) attempt() types.Attempt {
	a, ok := record[types.Attempt](w, lease.AttemptProjection, string(attID))
	if !ok {
		w.t.Fatal("no attempt")
	}
	return a
}

// fake runs the TypeScript fake adapter with node, as make test does inside
// the test sandbox.
func fake(t *testing.T, scenario string) session.Config {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH: the fake adapter tests run under make test (the test sandbox provides /opt/node/bin/node)")
	}
	root, err := filepath.Abs(filepath.Join("..", "..", "adapters"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "fake", "node_modules")); err != nil {
		t.Skip("the adapters workspace is not installed: run make ts-deps (make test does)")
	}
	return session.Config{Command: []string{node, filepath.Join(root, "fake", "src", "main.ts")},
		Env:      append([]string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + t.TempDir(), "ADC_FAKE_SCENARIO=" + scenario}, testguard.HomeEnv(t)...),
		Deadline: 30 * time.Second}
}

func spec(adapter session.Config) Spec {
	return Spec{Lease: leaID, LeaseRevision: 2, Attempt: attID, Base: base, Call: calID, Adapter: adapter, Prompt: []byte("fix it\n"),
		Prepare: types.AdapterPrepareParams{Role: "builder", Executor: "exe_01m3250vnfeyrv67x6w0ddqr6a", Model: "fake-model",
			Workspace:  types.AdapterPrepareParamsWorkspace{Path: "/workspace", Writable: true},
			Limits:     types.AdapterPrepareParamsLimits{MaxTurns: 8, MaxOutputTokens: 8000, WallSeconds: 60},
			ToolPolicy: types.AdapterPrepareParamsToolPolicy{Allowed: []string{"Edit"}, Mediated: []string{}}}}
}

// counted: the run's delta and final are recorded for this attempt and
// lease, adapter_report, whatever happened to its outputs (a result the
// session refused for a foreign handle included: Astra #108 finding 7).
func counted(t *testing.T, w *world, out Outcome) {
	t.Helper()
	ms := w.usage()
	if len(ms) != 2 || len(out.Usage) != 2 {
		t.Fatalf("%d measurements recorded, %d returned; want a delta and a final", len(ms), len(out.Usage))
	}
	for _, m := range ms {
		if m.Attempt != attID || m.Lease != leaID || m.SourceOfTruth != types.UsageMeasurementSourceOfTruthAdapterReport {
			t.Errorf("measurement %+v", m)
		}
	}
	calls, err := ledger.Reconcile(ms)
	if err != nil || calls[calID].Usage.In == 0 || calls[calID].Uncertain {
		t.Fatalf("reconciled %+v, %v", calls[calID], err)
	}
}

func TestSafeOutputIsPublished(t *testing.T) {
	w := newWorld(t)
	out, err := w.host.Run(context.Background(), spec(fake(t, "safe-output")))
	if err != nil || out.Session != nil {
		t.Fatalf("run: %v, session %v", err, out.Session)
	}
	if len(out.Refusals) != 0 || out.Submitted == nil || out.Submitted.Blob == nil || out.Submitted.Outcome != types.OutcomeFound {
		t.Fatalf("outcome %+v", out)
	}
	if !w.published(patch) || w.tmpFiles() != 0 {
		t.Fatalf("published %v, %d temp files", w.published(patch), w.tmpFiles())
	}
	kind := types.SubmittedResultKindValuePatch
	sum := sha256.Sum256([]byte(patch))
	blob := types.Digest("sha256:" + hex.EncodeToString(sum[:]))
	want := types.SubmittedResult{Call: calID, Outcome: types.OutcomeFound, Reason: vocab.ReasonCompletedWithFindings, Kind: &kind, Blob: &blob}
	if !reflect.DeepEqual(*out.Submitted, want) {
		t.Fatalf("submitted %+v, want %+v", *out.Submitted, want)
	}
	if a := w.attempt(); a.Status != types.AttemptStatusFinishing || a.Publication != types.AttemptPublicationClosed {
		t.Fatalf("attempt %s, %s", a.Status, a.Publication)
	}
	if w.untrustworthy() {
		t.Fatal("untrustworthy without a refusal")
	}
	counted(t, w, out)
}

// refused checks one refusal of the attempt's patch.diff with check, and
// that nothing was published or submitted.
func refused(t *testing.T, w *world, out Outcome, check types.AttemptOutputRefusalCheck) {
	t.Helper()
	if len(out.Refusals) != 1 {
		t.Fatalf("%d refusals, want 1: %+v (session %v)", len(out.Refusals), out.Refusals, out.Session)
	}
	r := out.Refusals[0]
	reason := types.AttemptOutputRefusalReasonUnsafeOutput
	if check == types.AttemptOutputRefusalCheckStaleAttempt {
		reason = types.AttemptOutputRefusalReasonStaleAttempt
	}
	if r.Check != check || r.Reason != reason || r.Attempt != attID || r.Handle.Handle != "patch.diff" || r.Detail == "" {
		t.Fatalf("refusal %+v, want check %s", r, check)
	}
	if out.Submitted != nil {
		t.Fatalf("submitted %+v despite the refusal", out.Submitted)
	}
	if !w.untrustworthy() {
		t.Fatal("not untrustworthy after a refusal")
	}
	if a := w.attempt(); a.Publication != types.AttemptPublicationClosed {
		t.Fatalf("attempt %s with publication %s", a.Status, a.Publication)
	}
	counted(t, w, out)
}

func TestUnsafeOutputsAreRefused(t *testing.T) {
	for scenario, check := range map[string]types.AttemptOutputRefusalCheck{
		"unsafe-symlink":  types.AttemptOutputRefusalCheckSymlink,
		"unsafe-fifo":     types.AttemptOutputRefusalCheckSpecialFile,
		"unsafe-hardlink": types.AttemptOutputRefusalCheckHardLink,
		"unsafe-digest":   types.AttemptOutputRefusalCheckDigest,
		"unsafe-replaced": types.AttemptOutputRefusalCheckDigest, // replaced before exit: a stable file of other bytes
		"foreign-handle":  types.AttemptOutputRefusalCheckForeignHandle,
	} {
		t.Run(scenario, func(t *testing.T) {
			w := newWorld(t)
			out, err := w.host.Run(context.Background(), spec(fake(t, scenario)))
			if err != nil {
				t.Fatal(err)
			}
			refused(t, w, out, check)
			if w.published(patch) {
				t.Fatal("the refused patch was published")
			}
			if a := w.attempt(); a.Status != types.AttemptStatusLost {
				t.Fatalf("attempt %s, want lost", a.Status)
			}
			if check == types.AttemptOutputRefusalCheckForeignHandle && out.Refusals[0].Handle.Attempt == attID {
				t.Fatal("the foreign refusal records the submitting attempt's id as the handle's")
			}
		})
	}
}

// An attempt stopped after its adapter exited is refused at the first fence,
// before any output is read (even an unsafe one: the fence comes first);
// one stopped after ingestion at the transaction's own fence, its staged
// copy discarded. Either way nothing is published, nothing stays staged,
// and its usage is counted.
func TestStaleAttemptIsRefusedButCounted(t *testing.T) {
	for _, tc := range []struct {
		stage, scenario string
		staged          int
	}{{"exited", "safe-output", 0}, {"exited", "unsafe-symlink", 0}, {"ingested", "safe-output", 1}} {
		t.Run(tc.stage+"/"+tc.scenario, func(t *testing.T) {
			w := newWorld(t)
			testHook = func(s string) {
				if s == tc.stage {
					w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
						return lease.ChangeAttempt(tx, attID, 1, lease.AttemptEvent{Kind: lease.StopRequested, StopReason: types.AttemptStopReasonValueRevoke})
					})
				}
			}
			staged := 0
			stage = func(dir *os.File, name, declared string, size, bound int64, store *blobstore.Store) (*blobstore.Staged, error) {
				staged++
				return ingest.Stage(dir, name, declared, size, bound, store)
			}
			t.Cleanup(func() { testHook, stage = nil, ingest.Stage })
			out, err := w.host.Run(context.Background(), spec(fake(t, tc.scenario)))
			if err != nil {
				t.Fatal(err)
			}
			refused(t, w, out, types.AttemptOutputRefusalCheckStaleAttempt)
			if a := w.attempt(); a.Status != types.AttemptStatusStopping {
				t.Fatalf("attempt %s, want stopping", a.Status)
			}
			if staged != tc.staged || w.published(patch) || w.tmpFiles() != 0 {
				t.Fatalf("staged %d (want %d), published %v, %d temp files", staged, tc.staged, w.published(patch), w.tmpFiles())
			}
		})
	}
}

// tmpFiles counts the blob store's staged and temporary files.
func (w *world) tmpFiles() int {
	left, err := os.ReadDir(filepath.Join(w.dir, "blobs", "tmp"))
	if err != nil {
		w.t.Fatal(err)
	}
	return len(left)
}

// A failed prelaunch check refuses the start: the lease is released, no
// attempt exists and the adapter is never launched.
func TestCapabilityCheckFailureLaunchesNothing(t *testing.T) {
	w := newWorld(t)
	probe = func(*os.File) error { return fmt.Errorf("%w: withheld by the test", ingest.ErrUnsupported) }
	t.Cleanup(func() { probe = ingest.SupportedIn })
	marker := filepath.Join(t.TempDir(), "launched")
	out, err := w.host.Run(context.Background(), spec(session.Config{Command: []string{"/bin/sh", "-c", "echo > " + marker},
		Env: []string{"PATH=/usr/bin:/bin"}, Deadline: 10 * time.Second}))
	if err != nil || !errors.Is(out.StartRefused, ingest.ErrUnsupported) {
		t.Fatalf("run: %v, start refused %v", err, out.StartRefused)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the adapter was launched")
	}
	l, _ := record[types.Lease](w, lease.LeaseProjection, string(leaID))
	if l.State != types.LeaseStateReleasing || l.Attempt != nil {
		t.Fatalf("lease %s, attempt %v", l.State, l.Attempt)
	}
	if _, ok := record[types.Attempt](w, lease.AttemptProjection, string(attID)); ok {
		t.Fatal("an attempt exists")
	}
	if len(w.usage()) != 0 || out.Result != nil {
		t.Fatal("something ran")
	}
	entries, err := os.ReadDir(w.host.Root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("the exchange root was left behind: %v %v", entries, err)
	}
}

// A start the writer commits as refused (14 N2) launches nothing: only an
// applied start advances the attempt.
func TestACommittedRefusalBeforeStartLaunchesNothing(t *testing.T) {
	w := newWorld(t)
	decided := 0
	decideStart = func(*eventstore.Tx, Spec) (eventstore.Decision, error) {
		decided++
		return eventstore.Decision{Disposition: eventstore.DispositionRefused,
			Result: json.RawMessage(`{"outcome":"failed","reason":"precondition_failed","detail":"refused by the test"}`)}, nil
	}
	t.Cleanup(func() { decideStart = startAttempt })
	marker := filepath.Join(t.TempDir(), "launched")
	out, err := w.host.Run(context.Background(), spec(session.Config{Command: []string{"/bin/sh", "-c", "echo > " + marker},
		Env: []string{"PATH=/usr/bin:/bin"}, Deadline: 10 * time.Second}))
	if !errors.Is(err, eventstore.ErrNotApplied) || decided != 1 {
		t.Fatalf("run: %v (decided %d)", err, decided)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the adapter was launched")
	}
	if _, ok := record[types.Attempt](w, lease.AttemptProjection, string(attID)); ok {
		t.Fatal("an attempt exists")
	}
	if l, _ := record[types.Lease](w, lease.LeaseProjection, string(leaID)); l.State != types.LeaseStateGranted || l.Attempt != nil {
		t.Fatalf("lease %s, attempt %v", l.State, l.Attempt)
	}
	if len(w.usage()) != 0 || out.Result != nil || out.Submitted != nil {
		t.Fatal("something ran")
	}
}

// anyUnit admits an attempt on every unit and binds none to a project and
// task: these tests run leases with no graph (M1 P8a's binding).
type anyUnit struct{}

func (anyUnit) ProjectTask(*eventstore.Tx, types.UnitID) (types.ProjectID, types.TaskID, bool, error) {
	return "", "", false, nil
}

func (anyUnit) Startable(*eventstore.Tx, types.UnitID) error { return nil }
