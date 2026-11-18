package brief

import (
	"strconv"
	"strings"
	"time"

	"hop.top/flo/internal/core"
)

// ParseSince reads a --since value: a duration back from now ("36h", or "7d"
// for days), an RFC 3339 instant, or a date (YYYY-MM-DD, UTC). Empty is the
// zero time: every retained output.
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n >= 0 {
			return now.AddDate(0, 0, -n), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, core.Errorf(core.CodeUsage, "--since %q: want a duration (36h, 7d), an RFC 3339 time or a date (2026-11-01)", s).
		WithSuggestion("flo brief show --since 24h")
}
