package triggers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"hop.top/kit/go/runtime/bus"

	"hop.top/flo/internal/core"
	specrun "hop.top/flo/spec/run"
)

func decodePayload(j core.Job) (core.TriggerFirePayload, error) {
	var p core.TriggerFirePayload
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		// a malformed payload never improves with a retry: drop the job
		return p, core.Wrap(core.CodeInternal, err)
	}
	return p, nil
}

// handleFire is the trigger.fire job: one schedule occurrence.
//
// The next occurrence is queued first, behind a compare-and-set on the
// registration, so a start that fails or a handler that runs twice cannot
// break the chain or queue it twice. Then the occurrence is judged: a fire
// that is on time, or late but still inside its own period (before the next
// occurrence), starts a run; each occurrence whose whole period has passed
// while the engine was down is dropped, recorded once as a missed fire.
func (s *Scheduler) handleFire(ctx context.Context, j core.Job) error {
	p, err := decodePayload(j)
	if err != nil {
		return nil //nolint:nilerr // see decodePayload
	}
	rec, err := s.Triggers.GetTrigger(ctx, p.Tenant, p.Trigger)
	if core.IsCode(err, core.CodeTriggerNotFound) || (err == nil && rec.State != core.TriggerActive) {
		return nil // removed or paused since it was queued
	}
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	sc, err := compile(rec)
	if err != nil {
		return err
	}
	now := s.Clock.Now()
	occ, dropped, lastDropped := p.At, 0, time.Time{}
	var next time.Time
	var more bool
	for {
		if next, more, err = sc.next(occ); err != nil {
			return err
		}
		if !more || now.Before(next) || dropped >= maxMissedWalk {
			break
		}
		dropped, lastDropped, occ = dropped+1, occ, next
	}

	var nextJob core.JobID
	var nextAt *time.Time
	if more {
		if nextJob, err = s.enqueueFire(ctx, rec.Tenant, rec.Flow, rec.ID, sc, next); err != nil {
			return err
		}
		nextAt = &next
	}
	won, err := s.Triggers.AdvanceTrigger(ctx, rec.Tenant, rec.ID, &p.At, nextAt, nextJob)
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	if won && more {
		s.emit(ctx, core.TopicTriggerScheduled, core.TriggerEvent{
			Tenant: rec.Tenant, Flow: rec.Flow, Trigger: rec.ID, Kind: specrun.TriggerSchedule, Key: fireKey(next), At: next,
		})
	}

	if dropped > 0 {
		reason := fmt.Sprintf("missed %d occurrence(s) while the engine was down, the last at %s", dropped, lastDropped.UTC().Format(time.RFC3339))
		s.record(ctx, rec, core.Fire{Key: fireKey(lastDropped), At: lastDropped, Outcome: core.FireMissed, Reason: reason},
			core.TopicTriggerSkipped, specrun.TriggerSchedule)
	}
	return s.start(ctx, rec, core.StartRequest{
		Tenant: rec.Tenant, Flow: rec.Flow, StartedBy: s.StartedBy, IdemKey: fireKey(occ),
		Trigger: core.TriggerInfo{Kind: specrun.TriggerSchedule, At: occ},
	}, fireKey(occ), occ)
}

// start runs the Starter and records what the fire came to. A flow that
// cannot start (not active, gone) is a recorded skip, not a job failure; any
// other error fails the job so it retries.
func (s *Scheduler) start(ctx context.Context, rec core.TriggerRecord, req core.StartRequest, key string, at time.Time) error {
	res, err := s.Starter.Start(ctx, req)
	if core.IsCode(err, core.CodeFlowNotActive) || core.IsCode(err, core.CodeFlowNotFound) {
		var ce *core.Error
		_ = errors.As(err, &ce)
		s.record(ctx, rec, core.Fire{Key: key, At: at, Outcome: core.FireSkipped, Reason: string(ce.Code)},
			core.TopicTriggerSkipped, req.Trigger.Kind)
		return nil
	}
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	f := core.Fire{Key: key, At: at, Run: res.Run.ID, Outcome: core.FireStarted}
	topic := core.TopicTriggerFired
	switch {
	case res.Skipped:
		f.Outcome, f.Reason, topic = core.FireSkipped, "overlap", core.TopicTriggerSkipped
	case !res.Created:
		f.Outcome = core.FireDuplicate
	}
	s.record(ctx, rec, f, topic, req.Trigger.Kind)
	return nil
}

func (s *Scheduler) record(ctx context.Context, rec core.TriggerRecord, f core.Fire, topic bus.Topic, kind specrun.TriggerKind) {
	f.Tenant, f.Flow, f.Trigger, f.RecordedAt = rec.Tenant, rec.Flow, rec.ID, s.Clock.Now()
	_ = s.Triggers.RecordFire(ctx, f)
	s.emit(ctx, topic, core.TriggerEvent{
		Tenant: rec.Tenant, Flow: rec.Flow, Trigger: rec.ID, Kind: kind, Key: f.Key, At: f.At,
		Outcome: f.Outcome, Reason: f.Reason, Run: f.Run,
	})
}
