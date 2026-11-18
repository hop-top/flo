package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/plugin"
	"hop.top/flo/internal/triggers"
	"hop.top/flo/spec/cel"
	specrun "hop.top/flo/spec/run"
)

// DefaultSyncInterval is how often a calendar trigger's source is re-read
// and its fires re-planned, unless config.calendar.sync_interval says
// otherwise.
const DefaultSyncInterval = 10 * time.Minute

// firesSeen is how many recorded fires of a trigger a sync reads to skip
// events already fired.
const firesSeen = 500

// Deps are the ports the Syncer works through.
type Deps struct {
	Clock    core.Clock
	Tenants  core.Tenants
	Triggers core.TriggerStore
	Jobs     core.Jobs
	Plugins  *plugin.Registry
	// Scheduler owns the FireMap the sync plans through.
	Scheduler *triggers.Scheduler
	// Config is config.calendar.* (sync_interval and the source settings the
	// plugin reads).
	Config map[string]any
}

// Syncer is the calendar.sync job: for one calendar trigger it polls the
// source, plans one delayed fire per upcoming event (FireMap.Plan), and
// queues its own next run. The chain survives a restart through Reconcile and
// starts when a flow with a calendar trigger is activated (Hook).
type Syncer struct{ Deps }

// NewSyncer returns a Syncer.
func NewSyncer(d Deps) *Syncer { return &Syncer{d} }

// Register installs the calendar.sync handler on the worker.
func (s *Syncer) Register(w core.JobWorker) { w.Handle(core.JobCalendarSync, s.handle) }

// Interval is the time between syncs of one trigger.
func (s *Syncer) Interval() time.Duration {
	switch v := s.Config["sync_interval"].(type) {
	case string:
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil && d > 0 {
			return d
		}
	case time.Duration:
		if v > 0 {
			return v
		}
	}
	return DefaultSyncInterval
}

func isCalendar(rec core.TriggerRecord) bool {
	return rec.Spec.Kind == specrun.TriggerCalendar && rec.State == core.TriggerActive
}

func (s *Syncer) handle(ctx context.Context, j core.Job) error {
	var p core.TriggerFirePayload
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return nil //nolint:nilerr // a malformed payload never improves with a retry
	}
	rec, err := s.Triggers.GetTrigger(ctx, p.Tenant, p.Trigger)
	if core.IsCode(err, core.CodeTriggerNotFound) || (err == nil && !isCalendar(rec)) {
		return nil // removed, paused or replaced since it was queued: the chain ends
	}
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	// the next sync first, so a failing source never breaks the chain
	if err := s.ensure(ctx, rec, j.ID, s.Clock.Now().Add(s.Interval())); err != nil {
		return err
	}
	_, err = s.SyncTrigger(ctx, rec)
	return err
}

// ensure queues a sync of rec at `at` unless one is already waiting (other
// than the job `self`, which is running).
func (s *Syncer) ensure(ctx context.Context, rec core.TriggerRecord, self core.JobID, at time.Time) error {
	jobs, err := s.Jobs.List(ctx, core.JobFilter{
		Type: core.JobCalendarSync, Tenant: rec.Tenant, Statuses: []core.JobStatus{core.JobPending, core.JobActive},
	})
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	for _, ji := range jobs {
		var p core.TriggerFirePayload
		if ji.ID != self && json.Unmarshal(ji.Payload, &p) == nil && p.Trigger == rec.ID {
			return nil
		}
	}
	spec := core.JobSpec{
		Type: core.JobCalendarSync, Tenant: rec.Tenant, Key: fmt.Sprintf("%s|%d", rec.ID, at.UnixNano()),
		Payload: core.TriggerFirePayload{Tenant: rec.Tenant, Flow: rec.Flow, Trigger: rec.ID, At: at},
	}
	if at.After(s.Clock.Now()) {
		spec.At = &at
	}
	_, err = s.Jobs.Enqueue(ctx, spec)
	return err //nolint:wrapcheck // port error passes through
}

// Ensure starts the sync chain of a calendar trigger now when none is
// running.
func (s *Syncer) Ensure(ctx context.Context, rec core.TriggerRecord) error {
	if !isCalendar(rec) {
		return nil
	}
	return s.ensure(ctx, rec, "", s.Clock.Now())
}

// Reconcile is the start-up pass: every active calendar trigger of every
// tenant gets its sync chain started if it has none.
func (s *Syncer) Reconcile(ctx context.Context) error {
	tenants, err := s.Tenants.List(ctx)
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	for _, t := range tenants {
		recs, err := s.Triggers.ListTriggers(ctx, t, "")
		if err != nil {
			return err //nolint:wrapcheck // port error passes through
		}
		for _, rec := range recs {
			if err := s.Ensure(ctx, rec); err != nil {
				return err
			}
		}
	}
	return nil
}

// SyncTrigger runs one sync of a calendar trigger: poll the source for the
// window the fires can lie in, then reconcile the planned fires with what it
// reported. It is idempotent: a second run over the same events changes
// nothing.
func (s *Syncer) SyncTrigger(ctx context.Context, rec core.TriggerRecord) (triggers.PlanResult, error) {
	var res triggers.PlanResult
	tp, ok := s.Plugins.Trigger(rec.Spec.Source)
	if !ok {
		return res, core.Errorf(core.CodeNodeNotFound, "calendar trigger %s: no plugin serves %q", rec.ID, rec.Spec.Source)
	}
	offset, err := parseDur(rec.Spec.Offset)
	if err != nil {
		return res, core.Wrap(core.CodeInvalidFlow, err)
	}
	horizon := specrun.DefaultHorizon
	if rec.Spec.Horizon != "" {
		if horizon, err = parseDur(rec.Spec.Horizon); err != nil || horizon <= 0 {
			return res, core.Errorf(core.CodeInvalidFlow, "calendar horizon %q is not a positive duration", rec.Spec.Horizon)
		}
	}
	accept, err := acceptor(rec.Spec.When)
	if err != nil {
		return res, err
	}
	// a fire lies in [now, now+horizon], or is already due: an event that
	// starts later than now but whose fire time has passed fires at once
	now := s.Clock.Now()
	from := now
	if offset > 0 {
		from = now.Add(-offset)
	}
	batch, err := tp.Poll(ctx, plugin.TriggerRequest{
		Tenant: rec.Tenant, Node: rec.Spec.Source, Options: rec.Spec.Options, Config: s.Config,
		From: from, To: now.Add(horizon - offset),
	})
	if err != nil {
		return res, err //nolint:wrapcheck // the plugin's own error
	}
	fired, err := s.fired(ctx, rec)
	if err != nil {
		return res, err
	}
	evs := make([]triggers.CalendarEvent, 0, len(batch.Events))
	for _, e := range batch.Events {
		ce, ok := calendarEvent(e)
		if !ok {
			continue
		}
		evs = append(evs, ce)
	}
	return s.Scheduler.FireMap().Plan(ctx, rec, evs, func(ev triggers.CalendarEvent) (bool, error) {
		if fired[firedKey(ev, offset)] {
			return false, nil // already fired for this start: a re-plan must not fire it again
		}
		if accept == nil {
			return true, nil
		}
		return accept(ev)
	})
}

// firedKey is the key a fire is recorded under: the planned key and the event
// start, as the calendar.fire handler forms it.
func firedKey(ev triggers.CalendarEvent, offset time.Duration) string {
	return "calendar:" + triggers.FireKey(ev, offset) + "|" + ev.Start.UTC().Format(time.RFC3339)
}

func (s *Syncer) fired(ctx context.Context, rec core.TriggerRecord) (map[string]bool, error) {
	fs, err := s.Triggers.ListFires(ctx, rec.Tenant, rec.Flow, rec.ID, firesSeen)
	if err != nil {
		return nil, err //nolint:wrapcheck // port error passes through
	}
	out := make(map[string]bool, len(fs))
	for _, f := range fs {
		out[f.Key] = true
	}
	return out, nil
}

func calendarEvent(e plugin.Event) (triggers.CalendarEvent, bool) {
	uid, _ := e.Payload["uid"].(string)
	start, err := time.Parse(time.RFC3339, fmt.Sprint(e.Payload["start"]))
	if uid == "" || err != nil {
		return triggers.CalendarEvent{}, false
	}
	rid, _ := e.Payload["recurrence_id"].(string)
	return triggers.CalendarEvent{UID: uid, RecurrenceID: rid, Start: start, Payload: e.Payload}, true
}

// acceptor compiles the trigger's `when`, judged against trigger.event.
func acceptor(when string) (func(triggers.CalendarEvent) (bool, error), error) {
	if strings.TrimSpace(when) == "" {
		return nil, nil //nolint:nilnil // no condition
	}
	env, err := cel.NewEnv()
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err)
	}
	cond, err := env.CompileCondition(when)
	if err != nil {
		return nil, core.Errorf(core.CodeInvalidFlow, "calendar when %q: %v", when, err)
	}
	return func(ev triggers.CalendarEvent) (bool, error) {
		return cond.Eval(cel.Activation{"trigger": map[string]any{"kind": "calendar", "event": ev.Payload}}) //nolint:wrapcheck // cel's own error
	}, nil
}

// parseDur reads a Go duration, also with a day unit ("7d", "-1d12h"), as the
// trigger fields of flow.md §6 are written.
func parseDur(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	i := strings.IndexByte(s, 'd')
	if i < 0 {
		d, err := time.ParseDuration(s)
		return d, err //nolint:wrapcheck // stdlib error names the input
	}
	head, tail := s[:i], s[i+1:]
	neg := strings.HasPrefix(head, "-")
	days, err := strconv.ParseFloat(strings.TrimLeft(head, "+-"), 64)
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

// Hook wraps a core.TriggerSync so that activating a flow also starts the sync
// chain of its calendar triggers; pausing needs nothing (a sync of a paused
// trigger ends its chain).
func (s *Syncer) Hook(inner core.TriggerSync) core.TriggerSync { return syncHook{inner, s} }

type syncHook struct {
	core.TriggerSync
	s *Syncer
}

func (h syncHook) Sync(ctx context.Context, tenant core.TenantID, v core.FlowVersion) error {
	if err := h.TriggerSync.Sync(ctx, tenant, v); err != nil {
		return err //nolint:wrapcheck // the scheduler's own error
	}
	recs, err := h.s.Triggers.ListTriggers(ctx, tenant, v.Name)
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	for _, rec := range recs {
		if err := h.s.Ensure(ctx, rec); err != nil {
			return err
		}
	}
	return nil
}
