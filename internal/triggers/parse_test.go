package triggers

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"": 0, "0": 0, "30m": 30 * time.Minute, "-30m": -30 * time.Minute, "7d": 7 * 24 * time.Hour,
		"-1d": -24 * time.Hour, "1d12h": 36 * time.Hour, "1.5d": 36 * time.Hour, "2h45m": 2*time.Hour + 45*time.Minute,
	} {
		got, err := parseDuration(in)
		if err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"x", "dd", "1x", "d"} {
		if _, err := parseDuration(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
