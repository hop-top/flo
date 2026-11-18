package calendar

import (
	"fmt"
	"strings"
	"time"

	vstar "hop.top/vstar"
)

const localLayout = "20060102T150405"

// propTime reads a DATE-TIME property: UTC (Z), local with a TZID, or
// floating (read in def). A TZID is an IANA name when the system knows it,
// else the calendar's VTIMEZONE (fixed offset: no DST change across a
// recurrence). allDay is true for a DATE value, which has no start instant.
func propTime(p vstar.Property, cal vstar.Calendar, def *time.Location) (t time.Time, allDay bool, err error) {
	v := strings.TrimSpace(p.Value)
	tzid := ""
	for _, pa := range p.Params {
		switch strings.ToUpper(pa.Name) {
		case "VALUE":
			allDay = allDay || strings.EqualFold(pa.Value, "DATE")
		case "TZID":
			tzid = pa.Value
		}
	}
	if allDay || len(v) == 8 {
		return time.Time{}, true, nil
	}
	if strings.HasSuffix(v, "Z") {
		if t, ok := vstar.ParseTime(v); ok {
			return t, false, nil
		}
		return time.Time{}, false, fmt.Errorf("%s %q: %w", p.Name, v, vstar.ErrMalformed)
	}
	if tzid == "" {
		t, err := time.ParseInLocation(localLayout, v, def)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("%s %q: %w", p.Name, v, vstar.ErrMalformed)
		}
		return t, false, nil
	}
	if loc, err := time.LoadLocation(tzid); err == nil {
		t, err := time.ParseInLocation(localLayout, v, loc)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("%s %q: %w", p.Name, v, vstar.ErrMalformed)
		}
		return t, false, nil
	}
	if t, ok := vstar.ParseTimeWithTZID(v, tzid, cal); ok {
		return t, false, nil
	}
	return time.Time{}, false, fmt.Errorf("%s %q: unknown TZID %q: %w", p.Name, v, tzid, vstar.ErrMalformed)
}

// propTimes reads a comma-separated DATE-TIME list (RDATE, EXDATE), skipping
// DATE values.
func propTimes(p vstar.Property, cal vstar.Calendar, def *time.Location) ([]time.Time, error) {
	var out []time.Time
	for _, part := range strings.Split(p.Value, ",") {
		q := p
		q.Value = part
		t, allDay, err := propTime(q, cal, def)
		if err != nil {
			return nil, err
		}
		if !allDay {
			out = append(out, t)
		}
	}
	return out, nil
}
