// Package calendar is the calendar pack (node.md §7, flow.md §6): a trigger
// plugin that reports upcoming VEVENTs from an ICS feed or a CalDAV
// collection, and the calendar.sync job that plans one delayed fire per
// event through the triggers FireMap.
//
// Events are decoded with the vstar RFC 5545 codec; recurrence is expanded
// with hop.top/vstar/rrule in the event's own zone, so a series keeps its
// wall-clock time across a DST change.
//
// Layout:
//
//	pack.yaml, nodes/   the embedded pack manifest and the trigger node spec
//	event.go            Event and its trigger.event payload
//	ical.go, ical_time.go  VCALENDAR to events (recurrence, overrides, zones)
//	source.go           Source port; ICS (file, URL) and crm stub sources
//	caldav.go           CalDAV calendar-query source
//	plugin.go           the calendar/events@1 trigger plugin
//	sync.go             the calendar.sync job: poll, plan, reschedule
package calendar

import (
	"embed"

	"hop.top/flo/spec/node"
)

// Namespace is the pack's name and its config namespace (config.calendar.*).
const Namespace = "calendar"

// EventsNode is the trigger node the plugin serves.
const EventsNode = "calendar/events@1"

//go:embed pack.yaml nodes/*.yaml
var files embed.FS

// Pack returns the embedded pack, parsed and checked.
func Pack() (*node.LoadedPack, error) { return node.LoadPack(files, "pack.yaml") }

// ConfigOf returns the pack's settings (config.calendar.*) out of the
// engine's expression config; nil when none are set.
func ConfigOf(exprConfig map[string]any) map[string]any {
	m, _ := exprConfig[Namespace].(map[string]any)
	return m
}
