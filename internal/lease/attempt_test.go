// SPDX-License-Identifier: Apache-2.0

package lease

import (
	"errors"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

var base = types.AttemptBase{Tree: types.GitID("git:" + "cccccccccccccccccccccccccccccccccccccccc"), ContractRevision: "7"}

func attemptEvent(kind string) AttemptEvent {
	e := AttemptEvent{Kind: kind}
	switch kind {
	case StopRequested:
		e.StopReason = types.AttemptStopReasonValueRevoke
	case Lost:
		e.LostReason = types.AttemptLostReasonValueStreamDead
	case Sealed:
		e.Checkpoint = dgA
	case AttemptDone:
		e.Completion = &types.AttemptCompletionValue{Outcome: types.Outcome(vocab.Empty), Reason: vocab.ReasonCompletedClean}
	}
	return e
}

var attemptKinds = []string{StopRequested, ResultSubmitted, Lost, Sealed, AttemptDone, Superseded}

// reachAttempt drives an attempt to status s.
func reachAttempt(t *testing.T, s types.AttemptStatus) types.Attempt {
	t.Helper()
	a := Start(attA, untID, leaID, base)
	paths := map[types.AttemptStatus][]string{
		types.AttemptStatusRunning: {}, types.AttemptStatusStopping: {StopRequested}, types.AttemptStatusFinishing: {ResultSubmitted},
		types.AttemptStatusLost: {Lost}, types.AttemptStatusSealed: {Lost, Sealed}, types.AttemptStatusCompleted: {Lost, Sealed, AttemptDone},
	}
	if s == types.AttemptStatusSuperseded {
		old, _, err := Supersede(reachAttempt(t, types.AttemptStatusSealed), attB, leaID, base)
		if err != nil {
			t.Fatal(err)
		}
		return old
	}
	for _, k := range paths[s] {
		var err error
		if a, err = ApplyAttempt(a, attemptEvent(k)); err != nil {
			t.Fatalf("reaching %s: %v", s, err)
		}
	}
	return a
}

// expectedAttempt is note 05's attempt table, written out independently of
// the code (superseded comes only from Supersede).
var expectedAttempt = map[attemptStep]types.AttemptStatus{
	{"running", StopRequested}: "stopping", {"running", ResultSubmitted}: "finishing", {"running", Lost}: "lost",
	{"stopping", Sealed}: "sealed", {"finishing", Sealed}: "sealed", {"lost", Sealed}: "sealed",
	{"sealed", AttemptDone}: "completed",
}

// Every (status, event) pair is allowed exactly as note 05's table says,
// every reachable attempt fits its schema, and publication is open only
// while running.
func TestAttemptTable(t *testing.T) {
	for _, s := range []types.AttemptStatus{types.AttemptStatusRunning, types.AttemptStatusStopping, types.AttemptStatusFinishing,
		types.AttemptStatusLost, types.AttemptStatusSealed, types.AttemptStatusCompleted, types.AttemptStatusSuperseded} {
		from := reachAttempt(t, s)
		encodes(t, "records/attempt", from)
		if (from.Publication == types.AttemptPublicationOpen) != (s == types.AttemptStatusRunning) {
			t.Errorf("%s with publication %s", s, from.Publication)
		}
		for _, k := range attemptKinds {
			got, err := ApplyAttempt(from, attemptEvent(k))
			want, ok := expectedAttempt[attemptStep{s, k}]
			switch {
			case ok && (err != nil || got.Status != want || got.Publication != types.AttemptPublicationClosed):
				t.Errorf("%s --%s--> %s/%s, %v; want %s/closed", s, k, got.Status, got.Publication, err, want)
			case !ok && !errors.Is(err, ErrTransition):
				t.Errorf("%s --%s--> accepted (%v)", s, k, err)
			case ok:
				encodes(t, "records/attempt", got)
			}
		}
	}
	for _, e := range []AttemptEvent{{Kind: StopRequested}, {Kind: Lost}} {
		if _, err := ApplyAttempt(Start(attA, untID, leaID, base), e); err == nil {
			t.Errorf("%s without its reason", e.Kind)
		}
	}
	if _, err := ApplyAttempt(reachAttempt(t, types.AttemptStatusLost), AttemptEvent{Kind: Sealed}); err == nil {
		t.Error("a seal without its AttemptRecord")
	}
	if _, err := ApplyAttempt(reachAttempt(t, types.AttemptStatusSealed), AttemptEvent{Kind: AttemptDone}); err == nil {
		t.Error("attempt.completed without its outcome")
	}
}

func stale(t *testing.T, err error) {
	t.Helper()
	var s *Stale
	if !errors.As(err, &s) || s.Reason() != vocab.ReasonStaleAttempt {
		t.Fatalf("want failed/stale_attempt, got %v", err)
	}
}

// 05: a revoked attempt's late result is refused, even before a
// replacement starts, and so is any other output but the current one's.
func TestFencing(t *testing.T) {
	a := Start(attA, untID, leaID, base)
	if err := Fence(a, attA, 1); err != nil {
		t.Fatalf("the current attempt: %v", err)
	}
	stale(t, Fence(a, attB, 1))
	stale(t, Fence(a, attA, 2))
	revoked, _ := ApplyAttempt(a, attemptEvent(StopRequested))
	stale(t, Fence(revoked, attA, 1))
	finishing, _ := ApplyAttempt(a, attemptEvent(ResultSubmitted))
	stale(t, Fence(finishing, attA, 1)) // finishing accepted only the one result
	lost := LoseOnRestart(a)
	if lost.Status != types.AttemptStatusLost || *lost.LostReason != types.AttemptLostReasonValueControllerRestart {
		t.Fatalf("restart left %s", lost.Status)
	}
	stale(t, Fence(lost, attA, 1))
	if got := LoseOnRestart(revoked); got.Status != types.AttemptStatusStopping {
		t.Errorf("restart changed a %s attempt", revoked.Status)
	}
}

// Supersession retires the old attempt and mints the next token in one
// step; the old attempt's outputs are then stale.
func TestSupersede(t *testing.T) {
	for _, s := range []types.AttemptStatus{types.AttemptStatusRunning, types.AttemptStatusStopping, types.AttemptStatusFinishing,
		types.AttemptStatusLost, types.AttemptStatusCompleted, types.AttemptStatusSuperseded} {
		if _, _, err := Supersede(reachAttempt(t, s), attB, leaID, base); !errors.Is(err, ErrTransition) {
			t.Errorf("superseding a %s attempt: %v", s, err)
		}
	}
	sealed := reachAttempt(t, types.AttemptStatusSealed)
	if _, _, err := Supersede(sealed, attA, leaID, base); err == nil {
		t.Error("a replacement reusing the old id")
	}
	old, next, err := Supersede(sealed, attB, leaID, base)
	if err != nil {
		t.Fatal(err)
	}
	encodes(t, "records/attempt", old)
	encodes(t, "records/attempt", next)
	if next.FencingToken != sealed.FencingToken+1 || *next.Supersedes != attA || *old.SupersededBy != attB {
		t.Fatalf("token %d, supersedes %v, by %v", next.FencingToken, next.Supersedes, old.SupersededBy)
	}
	if sealed.Status != types.AttemptStatusSealed {
		t.Error("Supersede changed its input")
	}
	stale(t, Fence(next, attA, sealed.FencingToken))
	if err := Fence(next, attB, next.FencingToken); err != nil {
		t.Fatalf("the replacement: %v", err)
	}
}
