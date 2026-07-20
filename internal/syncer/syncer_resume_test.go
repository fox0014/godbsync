package syncer

import (
	"math/big"
	"testing"
	"time"
)

// TestAnchorString verifies anchor string normalization: numbers keep string form, time uses RFC3339Nano.
func TestAnchorString(t *testing.T) {
	s := newTestSyncer()
	cases := []struct {
		in   interface{}
		want string
	}{
		{nil, ""},
		{int64(12345), "12345"},
		{int64(-7), "-7"},
		{"EMP001", "EMP001"},
		{new(big.Int).SetInt64(99999999999), "99999999999"},
		{time.Date(2026, 7, 11, 0, 30, 0, 123456789, time.UTC),
			"2026-07-11T00:30:00.123456789Z"},
	}
	for _, c := range cases {
		if got := s.anchorString(c.in, ""); got != c.want {
			t.Errorf("anchorString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
