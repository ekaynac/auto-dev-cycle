// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// The forced-handover clock, to the nanosecond: a request wins exactly at
// its force_after, and only when that force_after leaves the grace after
// the request's recorded time; an unparsable time never wins.
func TestForcedAtTheNanosecond(t *testing.T) {
	stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	req0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	request := func(after time.Time) seatRequest {
		a := types.Time(stamp(after))
		return seatRequest{Event: types.SeatEvent{ForceAfter: &a}, At: stamp(req0)}
	}
	after := req0.Add(SeatMinGrace)
	for name, c := range map[string]struct {
		req  seatRequest
		at   string
		want bool
	}{
		"1 ns before force_after":    {request(after), stamp(after.Add(-time.Nanosecond)), false},
		"at force_after":             {request(after), stamp(after), true},
		"after force_after":          {request(after), stamp(after.Add(time.Hour)), true},
		"force_after below floor":    {request(after.Add(-time.Nanosecond)), stamp(after.Add(time.Hour)), false},
		"no force_after":             {seatRequest{At: stamp(req0)}, stamp(after.Add(time.Hour)), false},
		"unparsable forcing time":    {request(after), "later", false},
		"unparsable request time":    {seatRequest{Event: request(after).Event, At: "now"}, stamp(after.Add(time.Hour)), false},
		"unparsable force_after":     {seatRequest{Event: types.SeatEvent{ForceAfter: ptrTime("soon")}, At: stamp(req0)}, stamp(after), false},
		"floor at exactly the grace": {request(after), stamp(after.Add(time.Nanosecond)), true},
	} {
		if got := forced(c.req, c.at, SeatMinGrace); got != c.want {
			t.Errorf("%s: forced = %t, want %t", name, got, c.want)
		}
	}
	// The request's own floor: below it refused, at it accepted.
	if graced(stamp(req0), stamp(after.Add(-time.Nanosecond)), SeatMinGrace) || !graced(stamp(req0), stamp(after), SeatMinGrace) {
		t.Error("the force_after floor is not exactly the grace")
	}
}

func ptrTime(s string) *types.Time { t := types.Time(s); return &t }
