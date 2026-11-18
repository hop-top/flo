package calendar

import (
	"sort"
	"time"
)

// Attendee is one ATTENDEE of an event.
type Attendee struct {
	Email, Name, Status string
}

// Event is one event instance: a single VEVENT, or one occurrence of a
// recurring one.
type Event struct {
	UID string
	// RecurrenceID is empty for a single event, else the original start of
	// the occurrence (RFC 3339, UTC), which stays stable when the
	// occurrence is moved.
	RecurrenceID string
	Start, End   time.Time
	Title        string
	Location     string
	Status       string
	Organizer    string
	Attendees    []Attendee
}

// ID is the stable id of the instance (flow.md §6.1 run key source).
func (e Event) ID() string { return e.UID + "|" + e.RecurrenceID }

// Payload is the trigger.event the run sees, matching the outputs.schema of
// calendar/events@1. It is JSON-shaped (stored with the planned fire).
func (e Event) Payload() map[string]any {
	att := make([]any, 0, len(e.Attendees))
	for _, a := range e.Attendees {
		att = append(att, map[string]any{"email": a.Email, "name": a.Name, "status": a.Status})
	}
	var rid any
	if e.RecurrenceID != "" {
		rid = e.RecurrenceID
	}
	return map[string]any{
		"uid": e.UID, "recurrence_id": rid,
		"start": e.Start.UTC().Format(time.RFC3339), "end": e.End.UTC().Format(time.RFC3339),
		"title": e.Title, "location": e.Location, "status": e.Status, "organizer": e.Organizer,
		"attendees": att,
	}
}

func sortEvents(evs []Event) {
	sort.SliceStable(evs, func(i, j int) bool {
		if !evs[i].Start.Equal(evs[j].Start) {
			return evs[i].Start.Before(evs[j].Start)
		}
		return evs[i].ID() < evs[j].ID()
	})
}
