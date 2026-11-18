package triggers

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"hop.top/vstar/rrule"

	"hop.top/flo/internal/core"
	specrun "hop.top/flo/spec/run"
)

// optDTStart is the reserved option under which a schedule registration keeps
// its anchor. An RRULE has no start of its own in `on:`, and the evaluator
// counts INTERVAL, COUNT and BYSETPOS from DTSTART, so the anchor must stay
// the same for the life of the registration: the start of the registration's
// day in the schedule's zone.
const optDTStart = "dtstart"

// maxMissedWalk bounds how many occurrences one late fire steps over.
const maxMissedWalk = 100000

// schedule is a compiled schedule trigger.
type schedule struct {
	id     core.TriggerID
	rule   rrule.Rule
	loc    *time.Location
	anchor time.Time // zero when the record has none
	jitter time.Duration
}

func compile(rec core.TriggerRecord) (*schedule, error) {
	sp := rec.Spec
	r, err := rrule.ParseRRule(sp.RRule)
	if err != nil {
		return nil, core.Errorf(core.CodeInvalidFlow, "schedule %q: %v", sp.RRule, err)
	}
	tz := sp.TZ
	if tz == "" {
		tz = specrun.DefaultTZ
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, core.Errorf(core.CodeInvalidFlow, "schedule tz %q: %v", tz, err)
	}
	s := &schedule{id: rec.ID, rule: r, loc: loc}
	if sp.Jitter != "" {
		if s.jitter, err = parseDuration(sp.Jitter); err != nil || s.jitter < 0 {
			return nil, core.Errorf(core.CodeInvalidFlow, "schedule jitter %q is not a duration", sp.Jitter)
		}
	}
	if v, ok := sp.Options[optDTStart].(string); ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			s.anchor = t
		}
	}
	return s, nil
}

func dayStart(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
}

// next returns the first occurrence strictly after `after`, in UTC, and false
// when the rule has ended. Sub-daily rules count from the start of the day of
// `after`: a registration's anchor would grow stale (the evaluator walks
// periods from DTSTART and stops at its cap) and a day divides evenly into
// the usual intervals.
func (s *schedule) next(after time.Time) (time.Time, bool, error) {
	dt := s.anchor.In(s.loc)
	if s.anchor.IsZero() || s.rule.Freq == rrule.FreqMinutely || s.rule.Freq == rrule.FreqHourly {
		dt = dayStart(after, s.loc)
	}
	t, ok, err := rrule.NextOccurrence(s.rule, dt, after.In(s.loc))
	if err != nil {
		return time.Time{}, false, core.Errorf(core.CodeInternal, "schedule next occurrence: %v", err)
	}
	return t.UTC(), ok, nil
}

// delay is the deterministic jitter of one occurrence: the same trigger and
// instant always wait the same, so a repeated enqueue lands on one time.
func (s *schedule) delay(at time.Time) time.Duration {
	if s.jitter <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(string(s.id) + "|" + at.UTC().Format(time.RFC3339)))
	return time.Duration(h.Sum64() % uint64(s.jitter))
}

// parseDuration reads a Go duration, also with a day unit ("7d", "-1d12h").
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if i := strings.IndexByte(s, 'd'); i >= 0 {
		head, tail := s[:i], s[i+1:]
		neg := strings.HasPrefix(head, "-")
		days, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimPrefix(head, "-"), "+"), 64)
		if err != nil {
			return 0, fmt.Errorf("duration %q: %w", s, err)
		}
		var rest time.Duration
		if tail != "" {
			if rest, err = time.ParseDuration(strings.TrimLeft(tail, "+-")); err != nil {
				return 0, fmt.Errorf("duration %q: %w", s, err)
			}
		}
		d := time.Duration(days*float64(24*time.Hour)) + rest
		if neg {
			d = -d
		}
		return d, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("duration %q: %w", s, err)
	}
	return d, nil
}

// specKey identifies a trigger declaration, ignoring the registration anchor,
// so a new version with the same declaration keeps its registration.
func specKey(sp specrun.Trigger) string {
	opts := map[string]any{}
	for k, v := range sp.Options {
		if k != optDTStart {
			opts[k] = v
		}
	}
	sp.Options = nil
	if len(opts) > 0 {
		sp.Options = opts
	}
	b, _ := json.Marshal(sp)
	return string(b)
}

// fireKey is the run key of a schedule occurrence: (flow, instant); the flow
// is the other half of the store's unique (tenant, flow, key).
func fireKey(at time.Time) string { return "schedule:" + at.UTC().Format(time.RFC3339) }

// jobKey is the dedupe key of a fire job: one per trigger and instant.
func jobKey(t core.TriggerID, at time.Time) string {
	return string(t) + "|" + at.UTC().Format(time.RFC3339)
}
