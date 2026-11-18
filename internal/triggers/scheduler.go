// Package triggers schedules flow triggers: schedule triggers fire from an
// RRULE (each fire enqueues the next occurrence), calendar triggers from a
// FireMap of pending fires per upcoming event. Both end in Starter.Start with
// a run key that makes a repeated fire return the first run.
package triggers

import (
	"bytes"
	"context"
	"errors"
	"time"

	"hop.top/kit/go/runtime/bus"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/cel"
	specrun "hop.top/flo/spec/run"
)

// Deps are the ports the scheduler works through.
type Deps struct {
	Clock    core.Clock
	IDs      core.IDs
	Tenants  core.Tenants
	Flows    core.FlowStore
	Triggers core.TriggerStore
	Jobs     core.Jobs
	Starter  core.Starter
	// RunLog receives the engine's trigger events; nil emits none.
	RunLog core.RunLog
	// StartedBy is the profile runs started by a fire carry; default "flo".
	StartedBy string
}

// Scheduler implements core.TriggerSync and handles the fire jobs.
type Scheduler struct{ Deps }

var _ core.TriggerSync = (*Scheduler)(nil)

// New returns a Scheduler.
func New(d Deps) *Scheduler {
	if d.StartedBy == "" {
		d.StartedBy = "flo"
	}
	return &Scheduler{d}
}

// Register installs the fire handlers on the worker.
func (s *Scheduler) Register(w core.JobWorker) {
	w.Handle(core.JobTriggerFire, s.handleFire)
	w.Handle(core.JobCalendarFire, s.handleCalendarFire)
}

func (s *Scheduler) emit(ctx context.Context, topic bus.Topic, e core.TriggerEvent) {
	if s.RunLog == nil {
		return
	}
	_ = s.RunLog.Emit(ctx, e.Tenant, "", core.NewEvent(s.Clock, s.IDs, topic, e.Tenant, "", e))
}

func decodeTriggers(v core.FlowVersion) ([]specrun.Trigger, error) {
	f, err := cel.ParseFlow(bytes.NewReader(v.Source), v.Name+".yaml")
	if err != nil {
		return nil, core.Wrap(core.CodeInvalidFlow, err)
	}
	set, err := specrun.Decode(f)
	if err != nil {
		return nil, core.Wrap(core.CodeInvalidFlow, err)
	}
	return set.Triggers, nil
}

// Sync implements core.TriggerSync: the flow's registrations come to match the
// version. A declaration the previous version also had keeps its registration
// and its queued fire; a new one is registered and, for a schedule, its first
// fire queued; one the version dropped is removed and its jobs cancelled. It
// is idempotent, which also makes it the repair pass of Reconcile.
func (s *Scheduler) Sync(ctx context.Context, tenant core.TenantID, v core.FlowVersion) error {
	if err := core.RequireTenant(tenant); err != nil {
		return err
	}
	declared, err := decodeTriggers(v)
	if err != nil {
		return err
	}
	existing, err := s.Triggers.ListTriggers(ctx, tenant, v.Name)
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	now := s.Clock.Now()
	used := map[core.TriggerID]bool{}
	var recs []core.TriggerRecord
	for i, tr := range declared {
		if tr.Kind.Reserved() {
			continue
		}
		rec, ok := matchRecord(existing, used, i, tr)
		if !ok {
			rec = core.TriggerRecord{ID: core.NewTriggerID(s.IDs), Tenant: tenant, Flow: v.Name, Index: i, Spec: tr}
			if tr.Kind == specrun.TriggerSchedule {
				rec.Spec.Options = map[string]any{optDTStart: dayStart(now, locOf(tr)).Format(time.RFC3339)}
			}
		}
		used[rec.ID] = true
		rec.Index, rec.Version, rec.Hash, rec.State = i, v.Version, v.Hash, core.TriggerActive
		recs = append(recs, rec)
	}
	removed := false
	for _, old := range existing {
		if !used[old.ID] {
			removed = true
			if err := s.cancelFires(ctx, old); err != nil {
				return err
			}
		}
	}
	if removed {
		if err := s.Triggers.DeleteTriggers(ctx, tenant, v.Name); err != nil {
			return err //nolint:wrapcheck // port error passes through
		}
	}
	for _, rec := range recs {
		if rec.Spec.Kind == specrun.TriggerSchedule {
			if err := s.arm(ctx, &rec, now); err != nil {
				return err
			}
		}
		if err := s.Triggers.PutTrigger(ctx, rec); err != nil {
			return err //nolint:wrapcheck // port error passes through
		}
	}
	return nil
}

func locOf(tr specrun.Trigger) *time.Location {
	tz := tr.TZ
	if tz == "" {
		tz = specrun.DefaultTZ
	}
	if l, err := time.LoadLocation(tz); err == nil {
		return l
	}
	return time.UTC
}

// matchRecord finds the registration that already stands for declaration i:
// the same position and declaration first, else the same declaration moved.
func matchRecord(existing []core.TriggerRecord, used map[core.TriggerID]bool, i int, tr specrun.Trigger) (core.TriggerRecord, bool) {
	key := specKey(tr)
	for pass := 0; pass < 2; pass++ {
		for _, e := range existing {
			if used[e.ID] || specKey(e.Spec) != key || (pass == 0 && e.Index != i) {
				continue
			}
			return e, true
		}
	}
	return core.TriggerRecord{}, false
}

// arm makes sure a schedule registration has its next fire queued. A fire
// still queued stays (when it is overdue the engine was down and the handler
// decides what to do with it); otherwise the next occurrence from now is
// queued.
func (s *Scheduler) arm(ctx context.Context, rec *core.TriggerRecord, now time.Time) error {
	sc, err := compile(*rec)
	if err != nil {
		return err
	}
	if rec.NextJob != "" && rec.NextAt != nil {
		if info, err := s.Jobs.Get(ctx, rec.NextJob); err == nil && !info.Status.Finished() {
			return nil
		}
	}
	rec.NextAt, rec.NextJob = nil, ""
	next, ok, err := sc.next(now)
	if err != nil || !ok {
		return err
	}
	id, err := s.enqueueFire(ctx, rec.Tenant, rec.Flow, rec.ID, sc, next)
	if err != nil {
		return err
	}
	rec.NextAt, rec.NextJob = &next, id
	s.emit(ctx, core.TopicTriggerScheduled, core.TriggerEvent{
		Tenant: rec.Tenant, Flow: rec.Flow, Trigger: rec.ID, Kind: specrun.TriggerSchedule, Key: fireKey(next), At: next,
	})
	return nil
}

func (s *Scheduler) enqueueFire(ctx context.Context, tenant core.TenantID, flow string, id core.TriggerID, sc *schedule, at time.Time) (core.JobID, error) {
	run := at.Add(sc.delay(at))
	return s.Jobs.Enqueue(ctx, core.JobSpec{
		Type: core.JobTriggerFire, Tenant: tenant, Key: jobKey(id, at), At: &run,
		Payload: core.TriggerFirePayload{Tenant: tenant, Flow: flow, Trigger: id, At: at},
	})
}

// cancelFires cancels the queued fire and the planned calendar fires of a
// registration.
func (s *Scheduler) cancelFires(ctx context.Context, rec core.TriggerRecord) error {
	var errs []error
	if rec.NextJob != "" {
		errs = append(errs, s.Jobs.Cancel(ctx, rec.NextJob))
		s.emit(ctx, core.TopicTriggerCancelled, core.TriggerEvent{Tenant: rec.Tenant, Flow: rec.Flow, Trigger: rec.ID, Kind: rec.Spec.Kind})
	}
	planned, err := s.Triggers.ListPlannedFires(ctx, rec.Tenant, rec.ID)
	errs = append(errs, err)
	for _, p := range planned {
		if p.Job != "" {
			errs = append(errs, s.Jobs.Cancel(ctx, p.Job))
		}
		errs = append(errs, s.Triggers.DeletePlannedFire(ctx, rec.Tenant, rec.ID, p.Key))
		s.emit(ctx, core.TopicTriggerCancelled, core.TriggerEvent{Tenant: rec.Tenant, Flow: rec.Flow, Trigger: rec.ID, Kind: rec.Spec.Kind, Key: p.Key, At: p.At})
	}
	return errors.Join(errs...)
}

// Unsync implements core.TriggerSync: it cancels the flow's queued fires and
// pauses its registrations. Sync of a later activation wakes them.
func (s *Scheduler) Unsync(ctx context.Context, tenant core.TenantID, flow string) error {
	if err := core.RequireTenant(tenant); err != nil {
		return err
	}
	recs, err := s.Triggers.ListTriggers(ctx, tenant, flow)
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	var errs []error
	for _, rec := range recs {
		if err := s.cancelFires(ctx, rec); err != nil {
			errs = append(errs, err)
			continue
		}
		rec.State, rec.NextAt, rec.NextJob = core.TriggerPaused, nil, ""
		errs = append(errs, s.Triggers.PutTrigger(ctx, rec))
	}
	return errors.Join(errs...)
}

// Reconcile is the startup pass: it syncs the active version of every flow of
// every tenant, which re-queues a schedule's lost next fire and restores a
// registration a crash left half written. Paused flows are left alone.
func (s *Scheduler) Reconcile(ctx context.Context) error {
	tenants, err := s.Tenants.List(ctx)
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	var errs []error
	for _, t := range tenants {
		flows, err := s.Flows.ListFlows(ctx, t)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, f := range flows {
			if f.Active != nil {
				errs = append(errs, s.Sync(ctx, t, *f.Active))
			}
		}
	}
	return errors.Join(errs...)
}
