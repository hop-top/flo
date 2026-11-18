package triggers_test

import (
	"errors"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/triggers"
	specrun "hop.top/flo/spec/run"
)

const cal = `  - calendar: { source: calendar/events@1, offset: -30m }`

func calRig(t *testing.T, on string) (*rig, core.TriggerRecord) {
	t.Helper()
	r := newRig(t, on, "concurrency: { max: 5 }")
	r.sync(t)
	return r, r.recs(t)[0]
}

func ev(uid string, start time.Time) triggers.CalendarEvent {
	return triggers.CalendarEvent{UID: uid, Start: start, Payload: map[string]any{"uid": uid, "summary": "standup"}}
}

func TestFireMapPlansMovesCancelsAndFiresPastNow(t *testing.T) {
	r, rec := calRig(t, cal)
	fm := r.s.FireMap()
	now := r.f.Clock.Now()
	a, b, c := ev("a", now.Add(2*time.Hour)), ev("b", now.Add(10*time.Minute)), ev("far", now.Add(30*24*time.Hour))

	res, err := fm.Plan(bg, rec, []triggers.CalendarEvent{a, b, c}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ka, kb := triggers.FireKey(a, -30*time.Minute), triggers.FireKey(b, -30*time.Minute)
	if len(res.Planned) != 2 || res.Planned[0] != ka || res.Planned[1] != kb {
		t.Fatalf("planned %v (the far event is beyond the 7d horizon)", res.Planned)
	}
	pend := r.f.Jobs.Pending(core.JobCalendarFire)
	if len(pend) != 2 {
		t.Fatalf("%d jobs", len(pend))
	}
	// b's fire time (now-20m) is past: it fires at once, a at start-30m
	if !pend[0].ScheduledAt.Equal(now) {
		t.Errorf("past fire is due now: %s", pend[0].ScheduledAt)
	}
	pa, ok, _ := r.f.DB.Triggers.GetPlannedFire(bg, T, rec.ID, ka)
	if !ok || !pa.At.Equal(a.Start.Add(-30*time.Minute)) || !pa.EventStart.Equal(a.Start) || pa.Job == "" {
		t.Fatalf("planned a = %+v", pa)
	}

	// same events again: nothing changes
	res, _ = fm.Plan(bg, rec, []triggers.CalendarEvent{a, b, c}, nil)
	if len(res.Kept) != 2 || len(res.Planned)+len(res.Moved)+len(res.Cancelled) != 0 {
		t.Errorf("replan unchanged = %+v", res)
	}
	if again, _, _ := r.f.DB.Triggers.GetPlannedFire(bg, T, rec.ID, ka); again.Job != pa.Job {
		t.Error("an unchanged fire keeps its job")
	}

	// a moves: cancel + enqueue; b is cancelled in the calendar
	a2 := a
	a2.Start = a.Start.Add(time.Hour)
	res, _ = fm.Plan(bg, rec, []triggers.CalendarEvent{a2}, nil)
	if len(res.Moved) != 1 || res.Moved[0] != ka || len(res.Cancelled) != 1 || res.Cancelled[0] != kb {
		t.Fatalf("replan = %+v", res)
	}
	if info, _ := r.f.Jobs.Get(bg, pa.Job); info.Status != core.JobCancelled {
		t.Errorf("old job = %s", info.Status)
	}
	moved, _, _ := r.f.DB.Triggers.GetPlannedFire(bg, T, rec.ID, ka)
	if moved.Job == pa.Job || !moved.At.Equal(a2.Start.Add(-30*time.Minute)) {
		t.Errorf("moved = %+v", moved)
	}
	if _, ok, _ := r.f.DB.Triggers.GetPlannedFire(bg, T, rec.ID, kb); ok {
		t.Error("cancelled event's fire is gone")
	}
	if live := r.f.Jobs.Pending(core.JobCalendarFire); len(live) != 1 || live[0].ID != moved.Job {
		t.Errorf("live jobs = %+v", live)
	}
	if r.count(core.TopicTriggerRescheduled) != 1 || r.count(core.TopicTriggerCancelled) != 1 || r.count(core.TopicTriggerScheduled) != 2 {
		t.Errorf("events = %v", r.topics())
	}
}

func TestFireMapWhenIsJudgedAtPlanTime(t *testing.T) {
	r, rec := calRig(t, cal)
	now := r.f.Clock.Now()
	keep, drop := ev("keep", now.Add(2*time.Hour)), ev("drop", now.Add(3*time.Hour))
	res, err := r.s.FireMap().Plan(bg, rec, []triggers.CalendarEvent{keep, drop}, func(e triggers.CalendarEvent) (bool, error) {
		return e.UID == "keep", nil
	})
	if err != nil || len(res.Planned) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	_, err = r.s.FireMap().Plan(bg, rec, []triggers.CalendarEvent{keep}, func(triggers.CalendarEvent) (bool, error) { return false, errors.New("bad when") })
	if err == nil {
		t.Error("a failing condition is reported")
	}
	if _, err := r.s.FireMap().Plan(bg, r.recs(t)[0], nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCalendarFireStartsOnceWithEventPayload(t *testing.T) {
	r, rec := calRig(t, cal)
	now := r.f.Clock.Now()
	e := ev("a", now.Add(10*time.Minute)) // fire time already past
	if _, err := r.s.FireMap().Plan(bg, rec, []triggers.CalendarEvent{e}, nil); err != nil {
		t.Fatal(err)
	}
	r.f.Worker.Drain(bg) // due now
	runs := r.runs(t)
	if len(runs) != 1 {
		t.Fatalf("%d runs", len(runs))
	}
	run := runs[0]
	ev, _ := run.Trigger.Payload["event"].(map[string]any)
	if run.Trigger.Kind != specrun.TriggerCalendar || ev["uid"] != "a" || run.IdemKey == "" {
		t.Errorf("run = %+v", run)
	}
	if left, _ := r.f.DB.Triggers.ListPlannedFires(bg, T, rec.ID); len(left) != 0 {
		t.Errorf("planned fire stays after it fired: %+v", left)
	}
	// the same event instance planned again (sync raced) fires but starts no second run
	if _, err := r.s.FireMap().Plan(bg, rec, []triggers.CalendarEvent{e}, nil); err != nil {
		t.Fatal(err)
	}
	r.f.Worker.Drain(bg)
	if got := r.runs(t); len(got) != 1 {
		t.Errorf("%d runs for one event instance", len(got))
	}
	fires, _ := r.f.DB.Triggers.ListFires(bg, T, "demo", rec.ID, 0)
	if len(fires) != 2 || fires[0].Outcome != core.FireDuplicate || fires[1].Outcome != core.FireStarted {
		t.Errorf("fires = %+v", fires)
	}
}

func TestCalendarFireSkippedWhenPlanWasCancelledOrTriggerPaused(t *testing.T) {
	r, rec := calRig(t, cal)
	now := r.f.Clock.Now()
	e := ev("a", now.Add(time.Hour))
	_, _ = r.s.FireMap().Plan(bg, rec, []triggers.CalendarEvent{e}, nil)
	h := captureHandlers{}
	r.s.Register(h)
	job := r.f.Jobs.Pending(core.JobCalendarFire)[0]
	_ = r.s.Unsync(bg, T, "demo") // cancels the job and the plan
	r.f.Clock.Advance(2 * time.Hour)
	if err := h[core.JobCalendarFire](bg, job.Job); err != nil {
		t.Fatal(err)
	}
	if len(r.runs(t)) != 0 {
		t.Error("a cancelled plan starts nothing")
	}
	if left, _ := r.f.DB.Triggers.ListPlannedFires(bg, T, rec.ID); len(left) != 0 {
		t.Errorf("unsync clears the plan: %+v", left)
	}
	// planning on a paused trigger is a no-op
	paused := r.recs(t)[0]
	if res, err := r.s.FireMap().Plan(bg, paused, []triggers.CalendarEvent{e}, nil); err != nil || len(res.Planned) != 0 {
		t.Errorf("paused plan = %+v %v", res, err)
	}
}

func TestFireMapOffsetAfterStartAndDayUnits(t *testing.T) {
	r, rec := calRig(t, `  - calendar: { source: calendar/events@1, offset: 1h, horizon: 1d }`)
	now := r.f.Clock.Now()
	started := ev("running", now.Add(-30*time.Minute)) // started, fire (+1h) still ahead
	over := ev("over", now.Add(-3*time.Hour))          // fire time passed too
	later := ev("later", now.Add(26*time.Hour))        // fire at +27h, past the 1d horizon
	res, err := r.s.FireMap().Plan(bg, rec, []triggers.CalendarEvent{started, over, later}, nil)
	if err != nil || len(res.Planned) != 1 || res.Planned[0] != triggers.FireKey(started, time.Hour) {
		t.Fatalf("%+v %v", res, err)
	}
}
