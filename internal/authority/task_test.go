// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// tasks is the boundary with the production project.create and
// task.create handlers, prjA and prjB registered by the operator, and an
// uploads store.
type tasks struct {
	*boundary
	up *blobstore.Store
}

func openTasks(t *testing.T) *tasks {
	b := openProjects(t, true)
	b.az.Handlers[authority.TaskCreateAction] = authority.TaskCreateHandler{}
	for _, id := range []types.ProjectID{prjA, prjB} {
		if _, _, err := b.createProject(b.createCmd(b.op, string(id), profileDoc(t, withID(id)))); err != nil {
			t.Fatal(err)
		}
	}
	up, err := blobstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &tasks{b, up}
}

// upload stores body as p's upload, uploaded at at, and returns its digest.
func (b *tasks) upload(p identity.Principal, body string, at time.Time) string {
	b.t.Helper()
	u, err := b.up.Upload(string(p.Actor()), strings.NewReader(body), at)
	if err != nil {
		b.t.Fatal(err)
	}
	return u.Digest
}

// spec is a task.create scope on project with the attachments, name then
// digest.
func spec(project types.ProjectID, title string, attachments ...string) []byte {
	var as []map[string]string
	for i := 0; i+1 < len(attachments); i += 2 {
		as = append(as, map[string]string{"name": attachments[i], "digest": attachments[i+1]})
	}
	return must(json.Marshal(map[string]any{"project": project, "title": title, "requirement": "make the hidden tests pass", "attachments": as}))
}

// create submits task.create as p under key, with its logic and, once
// committed, its drop; ran reports whether the logic ran.
func (b *tasks) create(p identity.Principal, key string, raw []byte) (out eventstore.Committed, ran bool, err error) {
	c := p.Command(newID(b.t, "cmd"), authority.TaskCreateAction, key, raw, nil)
	c.Expect = version(0)
	decide, drop := authority.TaskCreate{Uploads: b.up}.Request(c)
	out, err = b.s.Submit(context.Background(), c, func(tx *eventstore.Tx) (eventstore.Decision, error) {
		ran = true
		return decide(tx)
	})
	if err == nil {
		err = drop(out)
	}
	return out, ran, err
}

func (b *tasks) task(id string) (r authority.TaskRecord, ok bool) {
	b.t.Helper()
	b.view(func(tx *eventstore.Tx) error {
		raw, found, err := tx.Get(authority.TaskProjection, id)
		if err != nil || !found {
			return err
		}
		ok = true
		return json.Unmarshal(raw, &r)
	})
	return r, ok
}

func (b *tasks) events(n int) []eventstore.Event {
	b.t.Helper()
	var evs []eventstore.Event
	b.view(func(tx *eventstore.Tx) (err error) {
		evs, err = tx.EventsAfter(b.head()-int64(n), n)
		return err
	})
	return evs
}

func (b *tasks) live() []blobstore.Upload {
	b.t.Helper()
	us, err := b.up.Uploads()
	if err != nil {
		b.t.Fatal(err)
	}
	return us
}

// An operator's create records the task in Ready with its spec digest and
// its acceptance classed restricted from builder and reviewer, then, in the
// same command, approval.recorded binding that digest; the uploads are
// published, then dropped; a repeat replays; a repeat of the key with
// another spec is idempotency_key_reused; the tasks rebuild.
func TestTaskCreate(t *testing.T) {
	b := openTasks(t)
	a, h := b.upload(b.op, "def test_a(): pass\n", suiteNow()), b.upload(b.op, "def test_b(): pass\n", suiteNow())
	raw := spec(prjA, "split evenly", "hidden/a/test.py", a, "hidden/b/test.py", h)
	out, ran, err := b.create(b.op, "k", raw)
	if err != nil || !ran || out.Disposition != eventstore.DispositionApplied || out.LastSeq-out.FirstSeq != 1 {
		t.Fatalf("the create: %+v %v", out, err)
	}
	dg, err := eventstore.ContentDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	evs := b.events(2)
	if evs[0].Kind != authority.TaskCreated || evs[1].Kind != authority.ApprovalRecorded || *evs[0].Command != out.CommandID || *evs[1].Command != out.CommandID {
		t.Fatalf("the events: %+v", evs)
	}
	var apr types.Approval
	if err := json.Unmarshal(evs[1].Payload, &apr); err != nil || contract.Validate("records/approval", evs[1].Payload) != nil {
		t.Fatalf("the approval %s: %v", evs[1].Payload, err)
	}
	if apr.Proposal != nil || apr.Actor != b.op.Actor() || apr.Action != authority.TaskCreateAction || apr.Object.Type != "task" ||
		apr.Object.ID != evs[0].Subject || *apr.Object.Version != 0 || string(*apr.Object.Digest) != string(dg) ||
		apr.Confirmation.Policy != types.ApprovalConfirmationPolicyNone || apr.Grant != nil || *apr.Project != prjA {
		t.Fatalf("the approval %+v", apr)
	}
	r, ok := b.task(evs[0].Subject)
	if !ok || r.State != authority.TaskReady || string(r.SpecDigest) != string(dg) || r.Project != prjA || r.CreatedBy != b.op.Actor() ||
		r.Approval == nil || r.Approval.ID != apr.ID || string(r.Approval.Digest) != string(dg) || r.Approval.Status != authority.ApprovalInForce {
		t.Fatalf("the task %+v", r)
	}
	// The logic writes exactly TaskApprovalOf the task.
	if want := must(json.Marshal(authority.TaskApprovalOf(r.Task, raw, apr.ID, apr.At))); string(want) != string(must(json.Marshal(apr))) {
		t.Errorf("the approval %s, want %s", must(json.Marshal(apr)), want)
	}
	// The acceptance blobs are classed restricted, withheld from builder
	// and reviewer, as recorded.
	if len(r.Acceptance) != 2 || r.Acceptance[0].Name != "hidden/a/test.py" || r.Acceptance[1].Digest != h {
		t.Fatalf("the acceptance %+v", r.Acceptance)
	}
	for _, x := range r.Acceptance {
		if x.Class != types.AcceptanceBlobClassRestricted || len(x.WithheldFrom) != 2 ||
			x.WithheldFrom[0] != types.AcceptanceBlobWithheldFromItemBuilder || x.WithheldFrom[1] != types.AcceptanceBlobWithheldFromItemReviewer {
			t.Errorf("an acceptance blob not withheld from builder and reviewer: %+v", x)
		}
	}
	// Published into the store; the uploads dropped after the commit.
	for _, d := range []string{a, h} {
		if has, err := b.up.Has(d); err != nil || !has {
			t.Errorf("%s was not published: %v", d, err)
		}
	}
	if us := b.live(); len(us) != 0 {
		t.Errorf("the uploads were kept: %+v", us)
	}
	head := b.head()
	again, _, err := b.create(b.op, "k", raw)
	if err != nil || !again.Replayed || again.CommandID != out.CommandID || b.head() != head {
		t.Fatalf("the repeat: %+v %v", again, err)
	}
	other := spec(prjA, "another spec", "hidden/a/test.py", a)
	if _, ran, err := b.create(b.op, "k", other); !errors.Is(err, eventstore.ErrIdempotencyKeyReused) || ran || b.head() != head {
		t.Errorf("the key with another spec: %v (logic ran: %t)", err, ran)
	}
	b.rebuilds()
}

// A project holds at most five Ready tasks: a sixth is refused
// ready_backlog_full with no event and its upload kept; another project
// still takes one.
func TestTaskCreateBacklogBound(t *testing.T) {
	b := openTasks(t)
	for i := range authority.MaxReadyTasks {
		d := b.upload(b.op, "test "+string(rune('a'+i)), suiteNow())
		if _, _, err := b.create(b.op, "k"+string(rune('a'+i)), spec(prjA, "t", "test.py", d)); err != nil {
			t.Fatalf("task %d: %v", i, err)
		}
	}
	d := b.upload(b.op, "the sixth", suiteNow())
	h := b.head()
	if _, ran, err := b.create(b.op, "sixth", spec(prjA, "t", "test.py", d)); !isReason(vocab.ReasonReadyBacklogFull)(err) || !ran || b.head() != h {
		t.Fatalf("a sixth Ready task: %v", err)
	}
	if us := b.live(); len(us) != 1 || us[0].Digest != d {
		t.Errorf("the refused create's upload: %+v", us)
	}
	if _, _, err := b.create(b.op, "prjB", spec(prjB, "t", "test.py", d)); err != nil {
		t.Errorf("another project's first task: %v", err)
	}
	b.rebuilds()
}

// A revoked approval shows as revoked on the task: the approving operator
// is revoked; another operator's approval stays in force.
func TestTaskApprovalRevoked(t *testing.T) {
	b := openTasks(t)
	second := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "second", CreatedBy: b.op.Actor()}
	var tok identity.Secret
	b.seed(b.op, create(second))
	b.seed(b.op, issueTo(second.ID, b.op.Actor(), &tok))
	op2 := b.login(tok)
	out2, _, err := b.create(op2, "x", spec(prjA, "t", "test.py", b.upload(op2, "x", suiteNow())))
	if err != nil {
		t.Fatal(err)
	}
	out1, _, err := b.create(b.op, "y", spec(prjA, "t", "test.py", b.upload(b.op, "y", suiteNow())))
	if err != nil {
		t.Fatal(err)
	}
	b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, second.ID, suiteNow())
	})
	var t1, t2 struct{ Task string }
	if err := errors.Join(json.Unmarshal(out1.Result, &t1), json.Unmarshal(out2.Result, &t2)); err != nil {
		t.Fatal(err)
	}
	if r, _ := b.task(t2.Task); r.Approval == nil || r.Approval.Status != authority.ApprovalRevoked || r.Approval.RevokedAt == nil {
		t.Errorf("the revoked operator's approval: %+v", r.Approval)
	}
	if r, _ := b.task(t1.Task); r.Approval == nil || r.Approval.Status != authority.ApprovalInForce {
		t.Errorf("the other approval: %+v", r.Approval)
	}
	b.rebuilds()
}

// The logic runs after authority; it refuses an unregistered project and
// any attachment that is not the actor's own live upload
// (stale_expected_state). A supervisor needs a grant; a granted
// supervisor's task has no approval. A scope its schema or the unique
// names refuse is ErrInvalidScope, before anything runs.
func TestTaskCreateRefusals(t *testing.T) {
	b := openTasks(t)
	mine, theirs := b.upload(b.op, "mine", suiteNow()), b.upload(b.sup, "theirs", suiteNow())
	expired := b.upload(b.op, "expired", suiteNow().Add(-2*blobstore.UploadTTL))
	h := b.head()
	for name, raw := range map[string][]byte{
		"an unregistered project": spec(types.ProjectID(newID(t, "prj")), "t", "test.py", mine),
		"another actor's upload":  spec(prjA, "t", "test.py", theirs),
		"an expired upload":       spec(prjA, "t", "test.py", expired),
		"no upload":               spec(prjA, "t", "test.py", "sha256:"+strings.Repeat("0", 64)),
	} {
		if _, ran, err := b.create(b.op, name, raw); !errors.Is(err, eventstore.ErrStaleExpectedState) || !ran || b.head() != h {
			t.Errorf("%s: %v (logic ran: %t)", name, err, ran)
		}
	}
	for name, raw := range map[string][]byte{
		"a name twice":          spec(prjA, "t", "a.py", mine, "a.py", mine),
		"a file, then under it": spec(prjA, "t", "a", mine, "a/b.py", mine),
		"under a file, then it": spec(prjA, "t", "a/b/c.py", mine, "a/b", mine),
		"a 256-byte path":       spec(prjA, "t", strings.Repeat("a", 256), mine),
		"a parent segment":      spec(prjA, "t", "../a.py", mine),
		"an absolute path":      spec(prjA, "t", "/a.py", mine),
		"a hidden file":         spec(prjA, "t", "tests/.a.py", mine),
		"no attachments":        spec(prjA, "t"),
		"an unknown property":   []byte(`{"project":"` + string(prjA) + `","title":"t","requirement":"r","attachments":[],"admitted":true}`),
	} {
		if _, ran, err := b.create(b.op, name, raw); !errors.Is(err, authority.ErrInvalidScope) || ran || b.head() != h {
			t.Errorf("%s: %v (logic ran: %t)", name, err, ran)
		}
	}
	if _, ran, err := b.create(b.sys, "sys", spec(prjA, "t", "test.py", mine)); !isReason(vocab.ReasonActorKindRefused)(err) || ran {
		t.Errorf("the system: %v", err)
	}
	// b.supGrant covers note.add on prjA only.
	if _, ran, err := b.create(b.sup, "ungranted", spec(prjA, "t", "test.py", theirs)); !isReason(vocab.ReasonActionNotGranted)(err) || ran || b.head() != h {
		t.Errorf("an ungranted supervisor: %v (logic ran: %t)", err, ran)
	}
	g := b.supGrant
	g.ID, g.Actions = types.GrantID(newID(t, "grt")), []types.ActionKind{authority.TaskCreateAction}
	b.seed(b.op, createGrant(g))
	out, _, err := b.create(b.sup, "granted", spec(prjA, "t", "test.py", theirs))
	if err != nil || out.LastSeq != out.FirstSeq {
		t.Fatalf("a granted supervisor: %+v %v", out, err)
	}
	var res struct {
		Task     string
		Approval *string
	}
	if err := json.Unmarshal(out.Result, &res); err != nil || res.Approval != nil {
		t.Fatalf("the result %s: %v", out.Result, err)
	}
	if r, ok := b.task(res.Task); !ok || r.Approval != nil || r.CreatedBy != b.sup.Actor() {
		t.Errorf("the supervisor's task: %+v", r)
	}
	b.rebuilds()
}

// F (P3): an approval of another digest. The fold admits an approval with
// no proposal only as exactly the approval of the task's spec digest, by
// its operator, in its creating command: one binding another spec's digest,
// or the task's digest over another scope, is refused with its task, and
// so is one in a later command or of no task. The exact approval is
// admitted (positive control).
func TestTaskApprovalOfAnotherDigest(t *testing.T) {
	b := openTasks(t)
	d := b.upload(b.op, "x", suiteNow())
	raw, other := spec(prjA, "the spec", "test.py", d), spec(prjA, "another spec", "test.py", d)
	otherDigest, err := eventstore.ContentDigest(other)
	if err != nil {
		t.Fatal(err)
	}
	// created is the task.created event of raw, with the approval edit
	// makes of it (none: nil).
	created := func(by identity.Principal, id string, edit func(*types.Approval)) applyFn {
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			var in types.TaskCreateScope
			dg, err := eventstore.ContentDigest(raw)
			if err := errors.Join(err, json.Unmarshal(raw, &in)); err != nil {
				return nil, err
			}
			at := types.Time(tx.Now().Format(time.RFC3339Nano))
			task := types.Task{ID: id, Project: prjA, Spec: in, SpecDigest: dg, CreatedBy: by.Actor(), CreatedAt: at,
				Acceptance: []types.AcceptanceBlob{{Name: "test.py", Digest: d, Class: types.AcceptanceBlobClassRestricted,
					WithheldFrom: []types.AcceptanceBlobWithheldFromItem{types.AcceptanceBlobWithheldFromItemBuilder, types.AcceptanceBlobWithheldFromItemReviewer}}}}
			p := string(prjA)
			evs := []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.TaskCreated, Subject: id, Project: &p, Payload: must(json.Marshal(task))}}
			if edit == nil {
				return evs, nil
			}
			a := authority.TaskApprovalOf(task, raw, types.ApprovalID(newID(t, "apr")), at)
			edit(&a)
			return append(evs, eventstore.Event{ID: newID(t, "evt"), Kind: authority.ApprovalRecorded, Subject: string(a.ID), Project: &p, Payload: must(json.Marshal(a))}), nil
		}
	}
	h := b.head()
	for name, edit := range map[string]func(*types.Approval){
		"another spec's digest":          func(a *types.Approval) { a.Object.Digest = (*types.Digest)(&otherDigest) },
		"another scope under the digest": func(a *types.Approval) { a.Scope = other },
		"another spec, its own digest": func(a *types.Approval) {
			a.Scope, a.Object.Digest = other, (*types.Digest)(&otherDigest)
		},
		"a version other than 0": func(a *types.Approval) { v := int64(1); a.Object.Version = &v },
	} {
		if err := b.trySeed(b.op, created(b.op, newID(t, "tsk"), edit)); err == nil || !strings.Contains(err.Error(), "does not bind task") || b.head() != h {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The task alone, then its approval in a later command: refused.
	id := newID(t, "tsk")
	b.seed(b.op, created(b.op, id, nil))
	late := func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		r, _ := b.task(id)
		a := authority.TaskApprovalOf(r.Task, raw, types.ApprovalID(newID(t, "apr")), types.Time(tx.Now().Format(time.RFC3339Nano)))
		return authority.ApprovalEvent(a)
	}
	h = b.head()
	if err := b.trySeed(b.op, late); err == nil || b.head() != h {
		t.Errorf("an approval in a later command: admitted: %v", err)
	}
	// An approval with no proposal of no task.
	noTask := func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		r, _ := b.task(id)
		a := authority.TaskApprovalOf(r.Task, raw, types.ApprovalID(newID(t, "apr")), types.Time(tx.Now().Format(time.RFC3339Nano)))
		a.Object.ID = newID(t, "tsk")
		return authority.ApprovalEvent(a)
	}
	if err := b.trySeed(b.op, noTask); err == nil || b.head() != h {
		t.Errorf("an approval of no task: admitted: %v", err)
	}
	// Another live operator's approval in the creating command, and a
	// supervisor's own approval of its own task: refused.
	second := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "second", CreatedBy: b.op.Actor()}
	b.seed(b.op, create(second))
	g := b.supGrant
	g.ID, g.Actions = types.GrantID(newID(t, "grt")), []types.ActionKind{authority.TaskCreateAction}
	b.seed(b.op, createGrant(g))
	h = b.head()
	if err := b.trySeed(b.op, created(b.op, newID(t, "tsk"), func(a *types.Approval) { a.Actor = second.ID })); err == nil || b.head() != h {
		t.Errorf("another operator's approval: admitted: %v", err)
	}
	if err := b.trySeed(b.sup, created(b.sup, newID(t, "tsk"), func(*types.Approval) {})); err == nil || b.head() != h {
		t.Errorf("a supervisor's own approval: admitted: %v", err)
	}
	// The approval event filed under another project: refused.
	misfiled := func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := created(b.op, newID(t, "tsk"), func(*types.Approval) {})(tx)
		if err == nil {
			other := string(prjB)
			evs[1].Project = &other
		}
		return evs, err
	}
	if err := b.trySeed(b.op, misfiled); err == nil || !strings.Contains(err.Error(), "creating command") || b.head() != h {
		t.Errorf("an approval event in another project: %v", err)
	}
	// The exact approval, in the creating command, is admitted; another
	// task's approval under the same id is refused.
	exact, apr := newID(t, "tsk"), types.ApprovalID(newID(t, "apr"))
	b.seed(b.op, created(b.op, exact, func(a *types.Approval) { a.ID = apr }))
	if r, _ := b.task(exact); r.Approval == nil || r.Approval.Status != authority.ApprovalInForce {
		t.Fatalf("the exact approval: %+v", r)
	}
	h = b.head()
	if err := b.trySeed(b.op, created(b.op, newID(t, "tsk"), func(a *types.Approval) { a.ID = apr })); err == nil ||
		!strings.Contains(err.Error(), "already recorded") || b.head() != h {
		t.Errorf("a reused approval id: %v", err)
	}
	b.rebuilds()
}

// The fold refuses a task whose spec digest or acceptance classing is not
// its spec's.
func TestTaskFoldRefusesMisclassedAcceptance(t *testing.T) {
	b := openTasks(t)
	d := b.upload(b.op, "x", suiteNow())
	raw := spec(prjA, "the spec", "test.py", d)
	seedTask := func(edit func(*types.Task)) applyFn {
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			var in types.TaskCreateScope
			dg, err := eventstore.ContentDigest(raw)
			if err := errors.Join(err, json.Unmarshal(raw, &in)); err != nil {
				return nil, err
			}
			task := types.Task{ID: newID(t, "tsk"), Project: prjA, Spec: in, SpecDigest: dg, CreatedBy: b.op.Actor(),
				CreatedAt: types.Time(tx.Now().Format(time.RFC3339Nano)),
				Acceptance: []types.AcceptanceBlob{{Name: "test.py", Digest: d, Class: types.AcceptanceBlobClassRestricted,
					WithheldFrom: []types.AcceptanceBlobWithheldFromItem{types.AcceptanceBlobWithheldFromItemBuilder, types.AcceptanceBlobWithheldFromItemReviewer}}}}
			edit(&task)
			p := string(task.Project)
			return []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.TaskCreated, Subject: task.ID, Project: &p, Payload: must(json.Marshal(task))}}, nil
		}
	}
	h := b.head()
	for name, edit := range map[string]func(*types.Task){
		"another spec digest":     func(x *types.Task) { x.SpecDigest = types.Digest("sha256:" + strings.Repeat("1", 64)) },
		"another name":            func(x *types.Task) { x.Acceptance[0].Name = "other.py" },
		"another acceptance blob": func(x *types.Task) { x.Acceptance[0].Digest = types.Digest("sha256:" + strings.Repeat("2", 64)) },
	} {
		if err := b.trySeed(b.op, seedTask(edit)); err == nil || b.head() != h {
			t.Errorf("%s: admitted: %v", name, err)
		}
	}
	b.seed(b.op, seedTask(func(*types.Task) {}))
}

// task.created/v1 and its approval.recorded/v1, as stored, must keep
// replaying: frozen goldens (the actor and time filled in, as the fold
// requires them to be the command's) are admitted and rebuilt. A stored
// approval whose reason text differs from what TaskApprovalOf writes today
// replays too: the fold checks the binding fields, not today's builder.
func TestTaskGoldensReplay(t *testing.T) {
	task, err1 := os.ReadFile("testdata/task-created-v1.json")
	apr, err2 := os.ReadFile("testdata/approval-recorded-task-v1.json")
	if err := errors.Join(err1, err2); err != nil {
		t.Fatal(err)
	}
	const id, aprID = "tsk_01m3250vkwffc979nzjw0gtak1", "apr_01m3250vkwffc979nzjw0gapr1"
	for name, reason := range map[string]string{"as frozen": "", "another reason": "approved before the wording changed"} {
		t.Run(name, func(t *testing.T) {
			b := openTasks(t)
			b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				fill := strings.NewReplacer("@ACTOR@", string(b.op.Actor()), "@AT@", tx.Now().Format(time.RFC3339Nano))
				a := fill.Replace(string(apr))
				if reason != "" {
					a = strings.Replace(a, "an operator's task.create approves the spec it records", reason, 1)
				}
				p := string(prjA)
				return []eventstore.Event{
					{ID: newID(t, "evt"), Kind: authority.TaskCreated, Subject: id, Project: &p, Payload: []byte(fill.Replace(string(task)))},
					{ID: newID(t, "evt"), Kind: authority.ApprovalRecorded, Subject: aprID, Project: &p, Payload: []byte(a)},
				}, nil
			})
			if r, ok := b.task(id); !ok || r.Approval == nil || r.Approval.Status != authority.ApprovalInForce || string(r.Approval.ID) != aprID {
				t.Fatalf("the golden task %+v", r)
			}
			b.rebuilds()
		})
	}
}
