package calendar

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	vstar "hop.top/vstar"
	"hop.top/vstar/codec/rfc5545"
	"hop.top/vstar/duration"
	"hop.top/vstar/rrule"
)

// Window bounds the event starts a source reports: From inclusive, To
// exclusive.
type Window struct{ From, To time.Time }

func (w Window) has(t time.Time) bool { return !t.Before(w.From) && t.Before(w.To) }

// ParseICS decodes a VCALENDAR and returns the event instances whose start
// lies in w. A recurring event is expanded within w; RECURRENCE-ID
// overrides replace or (when cancelled) remove their occurrence; a
// STATUS:CANCELLED event yields nothing; an all-day event has no start
// instant and is skipped. A malformed event is skipped and reported in errs,
// the others still count. def is the zone of floating times.
func ParseICS(data []byte, w Window, def *time.Location) (events []Event, errs []error) {
	if def == nil {
		def = time.UTC
	}
	cal, err := rfc5545.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, []error{fmt.Errorf("parse calendar: %w", err)}
	}
	masters := map[string]vstar.Component{}
	overrides := map[string][]vstar.Component{}
	var order []string
	seen := map[string]bool{}
	for _, c := range cal.Filter(vstar.CompEvent) {
		uid := c.UID()
		if uid == "" {
			errs = append(errs, errors.New("event without UID skipped"))
			continue
		}
		if !seen[uid] {
			seen[uid] = true
			order = append(order, uid)
		}
		if _, ok := c.Get("RECURRENCE-ID"); ok {
			overrides[uid] = append(overrides[uid], c)
		} else {
			masters[uid] = c
		}
	}
	for _, uid := range order {
		master, hasMaster := masters[uid]
		var mp *vstar.Component
		if hasMaster {
			mp = &master
		}
		evs, err := expand(cal, mp, overrides[uid], w, def)
		if err != nil {
			errs = append(errs, fmt.Errorf("event %s: %w", uid, err))
		}
		events = append(events, evs...)
	}
	sortEvents(events)
	return events, errs
}

func expand(cal vstar.Calendar, master *vstar.Component, ovs []vstar.Component, w Window, def *time.Location) ([]Event, error) {
	var out []Event
	var errs []error
	moved := map[time.Time]bool{} // original instants an override covers
	for _, o := range ovs {
		rp, _ := o.Get("RECURRENCE-ID")
		rid, allDay, err := propTime(rp, cal, def)
		if err != nil || allDay {
			errs = append(errs, err)
			continue
		}
		moved[rid.UTC()] = true
		ev, ok, err := build(cal, o, def)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok && w.has(ev.Start) {
			ev.RecurrenceID = rid.UTC().Format(time.RFC3339)
			out = append(out, ev)
		}
	}
	if master == nil {
		return out, errors.Join(errs...)
	}
	base, ok, err := build(cal, *master, def)
	if err != nil {
		return out, errors.Join(append(errs, err)...)
	}
	if !ok {
		return out, errors.Join(errs...)
	}
	set, recurs, err := recurrence(cal, *master, base.Start, def)
	if err != nil {
		return out, errors.Join(append(errs, err)...)
	}
	if !recurs {
		if w.has(base.Start) {
			out = append(out, base)
		}
		return out, errors.Join(errs...)
	}
	if !w.To.After(w.From) {
		return out, errors.Join(errs...)
	}
	starts, err := set.Between(w.From, w.To)
	if err != nil {
		return out, errors.Join(append(errs, err)...)
	}
	length := base.End.Sub(base.Start)
	for _, s := range starts {
		if moved[s.UTC()] {
			continue
		}
		ev := base
		ev.Start, ev.End = s, s.Add(length)
		ev.RecurrenceID = s.UTC().Format(time.RFC3339)
		out = append(out, ev)
	}
	return out, errors.Join(errs...)
}

// recurrence returns the recurrence set of the component, in the zone of its
// DTSTART so wall-clock times hold across DST. recurs is false for a single
// event.
func recurrence(cal vstar.Calendar, c vstar.Component, start time.Time, def *time.Location) (set rrule.Set, recurs bool, err error) {
	set.DTStart = start
	if p, ok := c.Get("RRULE"); ok {
		r, err := rrule.ParseRRule(p.Value)
		if err != nil {
			return set, false, err //nolint:wrapcheck // vstar's own error
		}
		set.RRule, recurs = &r, true
	}
	for _, p := range c.GetAll("RDATE") {
		ts, err := propTimes(p, cal, def)
		if err != nil {
			return set, false, err
		}
		set.RDate, recurs = append(set.RDate, ts...), true
	}
	for _, p := range c.GetAll("EXDATE") {
		ts, err := propTimes(p, cal, def)
		if err != nil {
			return set, false, err
		}
		set.ExDate = append(set.ExDate, ts...)
	}
	return set, recurs, nil
}

// build reads one component as an event with its own DTSTART. ok is false for
// a cancelled or all-day event.
func build(cal vstar.Calendar, c vstar.Component, def *time.Location) (Event, bool, error) {
	ev := Event{UID: c.UID()}
	if p, ok := c.Get("STATUS"); ok {
		ev.Status = strings.ToUpper(strings.TrimSpace(p.Value))
	}
	if ev.Status == string(vstar.EventCancelled) {
		return ev, false, nil
	}
	sp, ok := c.Get("DTSTART")
	if !ok {
		return ev, false, errors.New("no DTSTART")
	}
	start, allDay, err := propTime(sp, cal, def)
	if err != nil {
		return ev, false, err
	}
	if allDay {
		return ev, false, nil
	}
	ev.Start, ev.End = start, start
	if p, ok := c.Get("DTEND"); ok {
		if end, endAllDay, err := propTime(p, cal, def); err == nil && !endAllDay {
			ev.End = end
		}
	} else if p, ok := c.Get("DURATION"); ok {
		if d, err := duration.Parse(strings.TrimSpace(p.Value)); err == nil {
			ev.End = d.AddTo(start)
		}
	}
	ev.Title, ev.Location = text(c, "SUMMARY"), text(c, "LOCATION")
	ev.Organizer = address(first(c, "ORGANIZER"))
	for _, p := range c.GetAll("ATTENDEE") {
		a := Attendee{Email: address(p)}
		for _, pa := range p.Params {
			switch strings.ToUpper(pa.Name) {
			case "CN":
				a.Name = pa.Value
			case "PARTSTAT":
				a.Status = strings.ToLower(pa.Value)
			}
		}
		ev.Attendees = append(ev.Attendees, a)
	}
	return ev, true, nil
}

func first(c vstar.Component, name string) vstar.Property {
	p, _ := c.Get(name)
	return p
}

func text(c vstar.Component, name string) string {
	p, _ := c.Get(name)
	return unescape(p.Value)
}

// address returns the address of a calendar-address property, without the
// mailto: scheme.
func address(p vstar.Property) string {
	v := strings.TrimSpace(p.Value)
	if len(v) >= 7 && strings.EqualFold(v[:7], "mailto:") {
		v = v[7:]
	}
	return v
}

var textEscapes = strings.NewReplacer(`\n`, "\n", `\N`, "\n", `\,`, ",", `\;`, ";", `\\`, `\`)

func unescape(s string) string { return textEscapes.Replace(s) }
