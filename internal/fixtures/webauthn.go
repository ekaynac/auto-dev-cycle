// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/authority/fakeauthn"
	"github.com/ekaynac/auto-dev-cycle/internal/authority/webauthn"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

var dashboard = webauthn.RelyingParty{ID: "adc.example", Origin: "https://adc.example"}

// humanState is what the writer persists for a human confirmation: the
// proposal, its challenge and the confirmer's credential.
type humanState struct {
	p    types.Proposal
	c    types.Challenge
	cred *webauthn.Credential
}

// confirm answers s's challenge with a and records the step. A
// confirmation that counts is persisted, as the writer would, and must
// record the counter a carries; a refusal must leave the proposal, the
// challenge and the credential exactly as they were. want, when set, is
// the refusal's intended cause (its detail).
func (o *observe) confirm(s *humanState, op types.Actor, a webauthn.Assertion, at time.Time, want string) error {
	before, err := json.Marshal([]any{s.p, s.c, *s.cred})
	if err != nil {
		return err
	}
	p, c, cred, err := authority.ConfirmHuman(s.p, s.c, op, *s.cred, dashboard, a, at)
	if err == nil {
		s.p, s.c, *s.cred = p, c, cred
	}
	if err := o.refused(err); err != nil {
		return err
	}
	after, merr := json.Marshal([]any{s.p, s.c, *s.cred})
	if merr != nil {
		return merr
	}
	if r := new(authority.Refusal); errors.As(err, &r) {
		o.check(bytes.Equal(before, after), "a refusal changed the proposal, challenge or credential: %s, now %s", before, after)
		o.check(want == "" || strings.Contains(r.Detail, want), "refused for %q, want %q", r.Detail, want)
	} else {
		count := binary.BigEndian.Uint32(a.AuthenticatorData[33:37])
		o.check(s.cred.SignCount == count, "a confirmation recorded counter %d, the assertion carries %d", s.cred.SignCount, count)
	}
	return nil
}

// humanProposal records a supervisor's data-widening profile edit, which
// authority proposes for a presence-verified human, and issues its
// challenge to op at issued.
func humanProposal(op types.Actor, issued time.Time, cred *webauthn.Credential) (*humanState, error) {
	reg, err := vocab.Load()
	if err != nil {
		return nil, err
	}
	sup, _, _, g, err := actors("profile.adopt")
	if err != nil {
		return nil, err
	}
	cur, project := adopted(), projectA
	d := authority.Decide(reg, authority.Request{Actor: sup, Action: "profile.adopt", Project: &project, Grants: []types.Grant{g}, Now: issued,
		Classes: authority.ClassifyProfile(&cur, edited(func(p *types.ProjectProfile) { p.DataPolicy.Floor = types.ProjectProfileDataPolicyFloorFreeUnknown }))})
	if d.Disposition != authority.Propose || d.Policy != authority.Human {
		return nil, fmt.Errorf("a data widening is %+v, not proposed for a human", d)
	}
	var idv [2]string
	for i, prefix := range []string{"prp", "chl"} {
		if idv[i], err = ids.New(prefix); err != nil {
			return nil, err
		}
	}
	p, err := authority.NewProposal(types.ProposalID(idv[0]), d, authority.Bound{Action: "profile.adopt", Project: &project,
		Object: types.ObjectRef{Type: "profile", ID: string(project)}, Scope: json.RawMessage(`{"edit":"` + idv[0] + `"}`), ProposedBy: sup.ID},
		issued, issued.Add(time.Hour))
	if err != nil {
		return nil, err
	}
	c, err := authority.IssueChallenge(types.ChallengeID(idv[1]), p, []types.Actor{op}, rand.Reader, issued)
	if err != nil {
		return nil, err
	}
	return &humanState{p: p, c: c, cred: cred}, nil
}

// replayedExpiredChallenge: an operator's fake authenticators answer
// human confirmation challenges on the dashboard. A fresh assertion
// confirms; the same assertion replayed, one for an expired challenge, one
// bound to another proposal's digest, a bad signature, a wrong RP ID or
// origin, and an assertion without user presence or verification are each
// refused and change nothing, so the second proposal's challenge still
// confirms it afterwards. A counterless credential (0/0, which the counter
// check accepts) confirms a third proposal; its replay is refused because
// the proposal is confirmed, and against the proposal still open, because
// its challenge was used (11 "a replayed/expired WebAuthn challenge (with
// a fake authenticator)"; 04 "Proposals and confirmation").
func replayedExpiredChallenge(context.Context, string) (steps []Step, violations []string, err error) {
	_, op, _, _, err := actors()
	if err != nil {
		return nil, nil, err
	}
	auth, err := fakeauthn.New(op.ID, dashboard, false)
	if err != nil {
		return nil, nil, err
	}
	still, err := fakeauthn.New(op.ID, dashboard, false)
	if err != nil {
		return nil, nil, err
	}
	still.Counter = fakeauthn.Counterless
	cred, zero := auth.Credential(), still.Credential()
	var states [3]*humanState
	for i, c := range []*webauthn.Credential{&cred, &cred, &zero} {
		if states[i], err = humanProposal(op, receiptsAt, c); err != nil {
			return nil, nil, err
		}
	}
	first, second, third := states[0], states[1], states[2]
	chl, err := ids.New("chl")
	if err != nil {
		return nil, nil, err
	}
	expiring := &humanState{p: second.p, cred: &cred}
	if expiring.c, err = authority.IssueChallenge(types.ChallengeID(chl), second.p, []types.Actor{op}, rand.Reader, receiptsAt); err != nil {
		return nil, nil, err
	}
	// assert answers s's challenge, bound to binding, from from with edit
	// applied to this assertion only.
	assert := func(from *fakeauthn.Authenticator, s *humanState, binding types.Digest, edit func(*fakeauthn.Authenticator)) (webauthn.Assertion, error) {
		ch, err := webauthn.Challenge(s.c.Nonce, binding)
		if err != nil {
			return webauthn.Assertion{}, err
		}
		f := *from
		edit(&f)
		a, err := f.Assert(ch)
		f.RPID, f.Origin, f.Flags = from.RPID, from.Origin, from.Flags
		*from = f // the counter advanced; the edit does not stay
		return a, err
	}
	honest := func(*fakeauthn.Authenticator) {}
	fresh, err := assert(auth, first, first.c.BindingDigest, honest)
	if err != nil {
		return nil, nil, err
	}
	var o observe
	if err := o.confirm(first, op, fresh, receiptsAt.Add(time.Minute), ""); err != nil {
		return nil, nil, err
	}
	o.check(first.p.Status == types.ProposalStatusConfirmed && first.c.UsedAt != nil && cred.SignCount == 1,
		"a fresh assertion left the proposal %s, the challenge used at %v and the counter at %d", first.p.Status, first.c.UsedAt, cred.SignCount)
	if err := o.confirm(first, op, fresh, receiptsAt.Add(2*time.Minute), "counter"); err != nil {
		return nil, nil, err
	}
	for _, c := range []struct {
		s       *humanState
		binding types.Digest
		edit    func(*fakeauthn.Authenticator)
		tamper  bool
		at      time.Time
		want    string
	}{
		{expiring, second.p.BindingDigest, honest, false, receiptsAt.Add(authority.ChallengeTTL), "expired"},
		{second, first.p.BindingDigest, honest, false, receiptsAt, "another challenge"},
		{second, second.p.BindingDigest, honest, true, receiptsAt, "signature"},
		{second, second.p.BindingDigest, func(f *fakeauthn.Authenticator) { f.RPID = "evil.example" }, false, receiptsAt, "RP ID"},
		{second, second.p.BindingDigest, func(f *fakeauthn.Authenticator) { f.Origin = "https://evil.example" }, false, receiptsAt, "origin"},
		{second, second.p.BindingDigest, func(f *fakeauthn.Authenticator) { f.Flags = webauthn.FlagUV }, false, receiptsAt, "not present"},
		{second, second.p.BindingDigest, func(f *fakeauthn.Authenticator) { f.Flags = webauthn.FlagUP }, false, receiptsAt, "not verified"},
		{second, second.p.BindingDigest, honest, false, receiptsAt, ""},
	} {
		a, err := assert(auth, c.s, c.binding, c.edit)
		if err != nil {
			return nil, nil, err
		}
		if c.tamper {
			a.Signature[len(a.Signature)/2] ^= 1
		}
		if err := o.confirm(c.s, op, a, c.at.Add(time.Minute), c.want); err != nil {
			return nil, nil, err
		}
	}
	o.check(second.p.Status == types.ProposalStatusConfirmed && second.c.UsedAt != nil && expiring.c.UsedAt == nil && cred.SignCount == 9,
		"the second proposal is %s, its challenge used at %v, its expired one at %v, the counter at %d",
		second.p.Status, second.c.UsedAt, expiring.c.UsedAt, cred.SignCount)
	// The counterless credential: confirm, then replay against the stored
	// (confirmed) proposal and against the proposal as it was while open.
	opened := *third
	zeroA, err := assert(still, third, third.c.BindingDigest, honest)
	if err != nil {
		return nil, nil, err
	}
	if err := o.confirm(third, op, zeroA, receiptsAt.Add(time.Minute), ""); err != nil {
		return nil, nil, err
	}
	o.check(third.p.Status == types.ProposalStatusConfirmed && zero.SignCount == 0, "a counterless confirmation left %s at counter %d", third.p.Status, zero.SignCount)
	if err := o.confirm(third, op, zeroA, receiptsAt.Add(2*time.Minute), "is confirmed"); err != nil {
		return nil, nil, err
	}
	opened.c = third.c
	if err := o.confirm(&opened, op, zeroA, receiptsAt.Add(2*time.Minute), "was used"); err != nil {
		return nil, nil, err
	}
	return o.steps, o.violations, nil
}
