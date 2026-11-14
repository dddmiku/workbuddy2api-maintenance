package trae

import (
	"encoding/json"
	"math"
	"testing"
)

func TestExactUsageIntegerBoundaries(t *testing.T) {
	for literal, want := range map[string]int64{
		"100.0": 100, "2e0": 2, "8e1": 80, "1.2300e2": 123, "1000e-3": 1,
		"0.0": 0, "0e-400": 0, "0e999999999999999999999": 0,
		"9223372036854775807.0": math.MaxInt64, "9.223372036854775807e18": math.MaxInt64,
	} {
		t.Run(literal, func(t *testing.T) {
			if got, ok := usageCount(json.Number(literal)); !ok || got != want {
				t.Fatalf("exact count got=%d valid=%t want=%d", got, ok, want)
			}
		})
	}
	for _, literal := range []string{"100.000000000000001", "9007199254740990.5", "1e-400", "-0", "-0.0", "-0e10", "-1", "-1e0", "9223372036854775808", "1e1000000", "1e999999999999999999999", "1/1", "+1", "01", " 1", "true"} {
		t.Run("reject/"+literal, func(t *testing.T) {
			if _, ok := usageCount(json.Number(literal)); ok {
				t.Fatal("invalid or out-of-range integer accepted")
			}
		})
	}
}

func TestBillCountExactRepresentationsAndSafeRange(t *testing.T) {
	for _, tc := range []struct {
		literal string
		want    int64
		valid   bool
	}{
		{"100.0", 100, true}, {"8e1", 80, true}, {"9007199254740991.0", maxSessionExactTokens, true},
		{"9007199254740992.0", 0, false}, {"100.000000000000001", 0, false}, {"-1", 0, false},
	} {
		t.Run(tc.literal, func(t *testing.T) {
			count := sessionBillCount(map[string]any{"n": json.Number(tc.literal)}, "n")
			if !count.Present || count.Valid != tc.valid || count.Value != tc.want {
				t.Fatalf("unexpected count metadata: %+v", count)
			}
		})
	}
}
