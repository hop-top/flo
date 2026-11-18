package calendar_test

import (
	"strings"
	"testing"
	"time"

	"hop.top/flo/internal/packs/calendar"
)

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// ics wraps events in a VCALENDAR with CRLF line ends.
func ics(body string) []byte {
	doc := "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//test//EN\n" + strings.TrimSpace(body) + "\nEND:VCALENDAR\n"
	return []byte(strings.ReplaceAll(doc, "\n", "\r\n"))
}

func win(from, to string) calendar.Window { return calendar.Window{From: utc(from), To: utc(to)} }

func starts(evs []calendar.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Start.UTC().Format(time.RFC3339))
	}
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}

func TestParseSingleEvent(t *testing.T) {
	evs, errs := calendar.ParseICS(ics(`
BEGIN:VEVENT
UID:one@x
DTSTART:20261120T100000Z
DTEND:20261120T110000Z
SUMMARY:Kickoff\, phase 1
LOCATION:Room 4
ORGANIZER;CN=Ann:mailto:ann@example.com
ATTENDEE;CN=Bob;PARTSTAT=ACCEPTED:mailto:bob@example.com
ATTENDEE:MAILTO:cy@example.com
END:VEVENT`), win("2026-11-18T00:00:00Z", "2026-11-25T00:00:00Z"), nil)
	if len(errs) != 0 || len(evs) != 1 {
		t.Fatalf("evs %v errs %v", evs, errs)
	}
	e := evs[0]
	if e.UID != "one@x" || e.RecurrenceID != "" || e.Title != "Kickoff, phase 1" || e.Location != "Room 4" || e.Organizer != "ann@example.com" {
		t.Errorf("event = %+v", e)
	}
	if !e.Start.Equal(utc("2026-11-20T10:00:00Z")) || !e.End.Equal(utc("2026-11-20T11:00:00Z")) {
		t.Errorf("times = %s %s", e.Start, e.End)
	}
	if len(e.Attendees) != 2 || e.Attendees[0] != (calendar.Attendee{Email: "bob@example.com", Name: "Bob", Status: "accepted"}) || e.Attendees[1].Email != "cy@example.com" {
		t.Errorf("attendees = %+v", e.Attendees)
	}
	p := e.Payload()
	if p["recurrence_id"] != nil || p["start"] != "2026-11-20T10:00:00Z" || len(p["attendees"].([]any)) != 2 {
		t.Errorf("payload = %+v", p)
	}
}

func TestParseSkipsCancelledAllDayAndOutsideWindow(t *testing.T) {
	evs, _ := calendar.ParseICS(ics(`
BEGIN:VEVENT
UID:cancelled
DTSTART:20261120T100000Z
STATUS:CANCELLED
END:VEVENT
BEGIN:VEVENT
UID:allday
DTSTART;VALUE=DATE:20261121
END:VEVENT
BEGIN:VEVENT
UID:later
DTSTART:20261201T100000Z
END:VEVENT
BEGIN:VEVENT
UID:kept
DTSTART:20261122T100000Z
DURATION:PT45M
END:VEVENT`), win("2026-11-18T00:00:00Z", "2026-11-25T00:00:00Z"), nil)
	if len(evs) != 1 || evs[0].UID != "kept" || !evs[0].End.Equal(utc("2026-11-22T10:45:00Z")) {
		t.Fatalf("events = %+v", evs)
	}
}

func TestParseBadEventDoesNotSinkTheOthers(t *testing.T) {
	evs, errs := calendar.ParseICS(ics(`
BEGIN:VEVENT
UID:bad
DTSTART:not-a-time
END:VEVENT
BEGIN:VEVENT
UID:good
DTSTART:20261122T100000Z
END:VEVENT`), win("2026-11-18T00:00:00Z", "2026-11-25T00:00:00Z"), nil)
	if len(evs) != 1 || evs[0].UID != "good" || len(errs) != 1 {
		t.Fatalf("evs %v errs %v", evs, errs)
	}
}

// A daily 09:00 Paris series keeps its wall-clock time across the spring DST
// change on 2027-03-28: 08:00Z before, 07:00Z after.
func TestRecurrenceIsDSTCorrect(t *testing.T) {
	evs, errs := calendar.ParseICS(ics(`
BEGIN:VEVENT
UID:daily@x
DTSTART;TZID=Europe/Paris:20270320T090000
DTEND;TZID=Europe/Paris:20270320T093000
RRULE:FREQ=DAILY
SUMMARY:Standup
END:VEVENT`), win("2027-03-26T00:00:00Z", "2027-03-30T00:00:00Z"), nil)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	eq(t, starts(evs), []string{"2027-03-26T08:00:00Z", "2027-03-27T08:00:00Z", "2027-03-28T07:00:00Z", "2027-03-29T07:00:00Z"})
	if evs[2].RecurrenceID != "2027-03-28T07:00:00Z" || !evs[2].End.Equal(utc("2027-03-28T07:30:00Z")) {
		t.Errorf("instance = %+v", evs[2])
	}
}

func TestRecurrenceExdateAndOverrides(t *testing.T) {
	evs, errs := calendar.ParseICS(ics(`
BEGIN:VEVENT
UID:w@x
DTSTART:20261116T090000Z
RRULE:FREQ=DAILY;COUNT=10
EXDATE:20261117T090000Z
SUMMARY:Series
END:VEVENT
BEGIN:VEVENT
UID:w@x
RECURRENCE-ID:20261118T090000Z
DTSTART:20261118T140000Z
SUMMARY:Series (moved)
END:VEVENT
BEGIN:VEVENT
UID:w@x
RECURRENCE-ID:20261119T090000Z
DTSTART:20261119T090000Z
STATUS:CANCELLED
END:VEVENT`), win("2026-11-16T00:00:00Z", "2026-11-21T00:00:00Z"), nil)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	// 16th kept, 17th excluded, 18th moved to 14:00, 19th cancelled, 20th kept
	eq(t, starts(evs), []string{"2026-11-16T09:00:00Z", "2026-11-18T14:00:00Z", "2026-11-20T09:00:00Z"})
	if evs[1].Title != "Series (moved)" || evs[1].RecurrenceID != "2026-11-18T09:00:00Z" {
		t.Errorf("moved instance = %+v", evs[1])
	}
}

func TestVTimezoneFallbackAndFloating(t *testing.T) {
	evs, errs := calendar.ParseICS(ics(`
BEGIN:VTIMEZONE
TZID:Corp Standard Time
BEGIN:STANDARD
DTSTART:19701101T020000
TZOFFSETFROM:+0100
TZOFFSETTO:+0100
END:STANDARD
END:VTIMEZONE
BEGIN:VEVENT
UID:corp
DTSTART;TZID=Corp Standard Time:20261120T100000
END:VEVENT
BEGIN:VEVENT
UID:float
DTSTART:20261120T100000
END:VEVENT`), win("2026-11-18T00:00:00Z", "2026-11-25T00:00:00Z"), time.FixedZone("x", 2*3600))
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	eq(t, starts(evs), []string{"2026-11-20T08:00:00Z", "2026-11-20T09:00:00Z"})
}
