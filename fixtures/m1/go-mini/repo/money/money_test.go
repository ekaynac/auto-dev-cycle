package money

import (
	"errors"
	"math"
	"testing"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]Cents{"12.34": 1234, "-0.5": -50, "7": 700, "0.05": 5, "-3.10": -310} {
		if got, err := Parse(in); err != nil || got != want {
			t.Errorf("Parse(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-", "1.", ".5", "1.234", "1,5", "1e3", "+1", "--1"} {
		if _, err := Parse(in); !errors.Is(err, ErrSyntax) {
			t.Errorf("Parse(%q): %v; want ErrSyntax", in, err)
		}
	}
	if _, err := Parse("99999999999999999999"); !errors.Is(err, ErrOverflow) {
		t.Errorf("a huge amount: %v", err)
	}
}

func TestString(t *testing.T) {
	for c, want := range map[Cents]string{1234: "12.34", -50: "-0.50", 700: "7.00", 5: "0.05", 0: "0.00", math.MinInt64: "-92233720368547758.08"} {
		if got := c.String(); got != want {
			t.Errorf("%d.String() = %q; want %q", int64(c), got, want)
		}
	}
}

func TestAdd(t *testing.T) {
	if got, err := Add(1234, -34); err != nil || got != 1200 {
		t.Errorf("Add: %d, %v", got, err)
	}
	if _, err := Add(math.MaxInt64, 1); !errors.Is(err, ErrOverflow) {
		t.Errorf("past the maximum: %v", err)
	}
	if _, err := Add(math.MinInt64, -1); !errors.Is(err, ErrOverflow) {
		t.Errorf("past the minimum: %v", err)
	}
}

func TestRoundTrip(t *testing.T) {
	for _, s := range []string{"12.34", "-0.50", "7.00", "0.05"} {
		c, err := Parse(s)
		if err != nil || c.String() != s {
			t.Errorf("%q: %d, %v", s, c, err)
		}
	}
}
