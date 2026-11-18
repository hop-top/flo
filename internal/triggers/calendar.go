package triggers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"hop.top/flo/internal/core"
	specrun "hop.top/flo/spec/run"
)

// CalendarEvent is one upcoming event as the calendar pack reports it.
type CalendarEvent struct {
	UID string
	// RecurrenceID is empty for a single event, else the instance's id.
	RecurrenceID string
	Start        time.Time
	// Payload is the `event` the run sees as trigger.event.
	Payload map[string]any
}

// FireMap keeps one pending fire per upcoming event of a calendar trigger,
// keyed by (UID, recurrence id, offset), each a delayed job (flow.md §6). A
// moved event cancels its job and queues a new one; a cancelled one is
// dropped; a fire time already in the past fires at once.
type FireMap struct{ *Scheduler }

// FireMap returns the calendar fire map of the scheduler.
func (s *Scheduler) FireMap() *FireMap { return &FireMap{s} }

// PlanResult says what Plan did, by planned-fire key.
type PlanResult struct {
	Planned, Moved, Cancelled, Kept []string
}

// FireKey is the planned-fire key of an event for a trigger offset.
func FireKey(ev CalendarEvent, offset time.Duration) string {
	return fmt.Sprintf("%s|%s|%s", ev.UID, ev.RecurrenceID, offset)
}

// Plan reconciles the trigger's pending fires with events, the complete list
// of upcoming events from the latest sync: a fire is planned for each event
// whose fire time (start plus the trigger's offset) lies within the horizon.
// accept, when not nil, is the trigger's `when`, judged now, when the fire is
// planned and not when it fires; a refused event gets no fire.
func (m *FireMap) Plan(ctx context.Context, rec core.TriggerRecord, events []CalendarEvent, accept func(CalendarEvent) (bool, error)) (PlanResult, error) {
	var res PlanResult
	if rec.Spec.Kind != specrun.TriggerCalendar {
		return res, core.Errorf(core.CodeUsage, "trigger %s is %s, not calendar", rec.ID, rec.Spec.Kind)
	}
	if rec.State != core.TriggerActive {
		return res, nil
	}
	offset, err := parseDuration(rec.Spec.Offset)
	if err != nil {
		return res, core.Wrap(core.CodeInvalidFlow, err)
	}
	horizon := specrun.DefaultHorizon
	if rec.Spec.Horizon != "" {
		if horizon, err = parseDuration(rec.Spec.Horizon); err != nil || horizon <= 0 {
			return res, core.Errorf(core.CodeInvalidFlow, "calendar horizon %q is not a positive duration", rec.Spec.Horizon)
		}
	}
	now := m.Clock.Now()
	pending, err := m.Triggers.ListPlannedFires(ctx, rec.Tenant, rec.ID)
	if err != nil {
		return res, err //nolint:wrapcheck // port error passes through
	}
	have := map[string]core.PlannedFire{}
	for _, p := range pending {
		have[p.Key] = p
	}

	var errs []error
	want := map[string]bool{}
	for _, ev := range events {
		at := ev.Start.Add(offset)
		// upcoming: the event has not started, or its fire is still ahead
		if (!ev.Start.After(now) && at.Before(now)) || at.After(now.Add(horizon)) {
			continue
		}
		if accept != nil {
			ok, err := accept(ev)
			if err != nil {
				errs = append(errs, fmt.Errorf("event %s: %w", ev.UID, err))
				continue
			}
			if !ok {
				continue
			}
		}
		key := FireKey(ev, offset)
		want[key] = true
		old, exists := have[key]
		planned := core.PlannedFire{
			Tenant: rec.Tenant, Flow: rec.Flow, Trigger: rec.ID, Key: key, At: at, EventStart: ev.Start, Payload: ev.Payload,
		}
		if exists && old.At.Equal(at) && old.EventStart.Equal(ev.Start) && m.queued(ctx, old.Job) {
			planned.Job = old.Job
			errs = append(errs, m.Triggers.PutPlannedFire(ctx, planned)) // payload may have changed
			res.Kept = append(res.Kept, key)
			continue
		}
		if exists && old.Job != "" {
			errs = append(errs, m.Jobs.Cancel(ctx, old.Job))
		}
		id, err := m.enqueueCalendar(ctx, rec, key, at, now)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		planned.Job = id
		if err := m.Triggers.PutPlannedFire(ctx, planned); err != nil {
			errs = append(errs, err)
			continue
		}
		topic, kind := core.TopicTriggerScheduled, &res.Planned
		if exists {
			topic, kind = core.TopicTriggerRescheduled, &res.Moved
		}
		*kind = append(*kind, key)
		m.emit(ctx, topic, core.TriggerEvent{Tenant: rec.Tenant, Flow: rec.Flow, Trigger: rec.ID, Kind: specrun.TriggerCalendar, Key: key, At: at})
	}
	for _, p := range pending {
		if want[p.Key] {
			continue
		}
		if p.Job != "" {
			errs = append(errs, m.Jobs.Cancel(ctx, p.Job))
		}
		errs = append(errs, m.Triggers.DeletePlannedFire(ctx, rec.Tenant, rec.ID, p.Key))
		res.Cancelled = append(res.Cancelled, p.Key)
		m.emit(ctx, core.TopicTriggerCancelled, core.TriggerEvent{Tenant: rec.Tenant, Flow: rec.Flow, Trigger: rec.ID, Kind: specrun.TriggerCalendar, Key: p.Key, At: p.At})
	}
	for _, l := range [][]string{res.Planned, res.Moved, res.Cancelled, res.Kept} {
		sort.Strings(l)
	}
	return res, errors.Join(errs...)
}

func (m *FireMap) queued(ctx context.Context, id core.JobID) bool {
	if id == "" {
		return false
	}
	info, err := m.Jobs.Get(ctx, id)
	return err == nil && !info.Status.Finished()
}

// enqueueCalendar queues the fire job. The job key carries the trigger, as
// two triggers can plan the same key. A fire time in the past runs now.
func (m *FireMap) enqueueCalendar(ctx context.Context, rec core.TriggerRecord, key string, at, now time.Time) (core.JobID, error) {
	spec := core.JobSpec{
		Type: core.JobCalendarFire, Tenant: rec.Tenant, Key: string(rec.ID) + "|" + key,
		Payload: core.TriggerFirePayload{Tenant: rec.Tenant, Flow: rec.Flow, Trigger: rec.ID, At: at, Key: key},
	}
	if at.After(now) {
		spec.At = &at
	}
	return m.Jobs.Enqueue(ctx, spec)
}

// handleCalendarFire is the calendar.fire job: it starts the run of one
// planned fire and clears the plan. The run key adds the event start to the
// planned key, so the same event instance never starts twice.
func (s *Scheduler) handleCalendarFire(ctx context.Context, j core.Job) error {
	p, err := decodePayload(j)
	if err != nil {
		return nil //nolint:nilerr // see decodePayload
	}
	pf, ok, err := s.Triggers.GetPlannedFire(ctx, p.Tenant, p.Trigger, p.Key)
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	if !ok {
		return nil // cancelled or moved since it was queued
	}
	rec, err := s.Triggers.GetTrigger(ctx, p.Tenant, p.Trigger)
	if core.IsCode(err, core.CodeTriggerNotFound) || (err == nil && rec.State != core.TriggerActive) {
		return nil
	}
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	key := "calendar:" + pf.Key + "|" + pf.EventStart.UTC().Format(time.RFC3339)
	if err := s.start(ctx, rec, core.StartRequest{
		Tenant: rec.Tenant, Flow: rec.Flow, StartedBy: s.StartedBy, IdemKey: key,
		Trigger: core.TriggerInfo{Kind: specrun.TriggerCalendar, At: pf.At, Payload: map[string]any{"event": pf.Payload}},
	}, key, pf.At); err != nil {
		return err
	}
	return s.Triggers.DeletePlannedFire(ctx, p.Tenant, p.Trigger, p.Key) //nolint:wrapcheck // port error passes through
}
