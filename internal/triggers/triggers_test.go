package triggers_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"hop.top/kit/go/runtime/bus"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/triggers"
	specrun "hop.top/flo/spec/run"
)

const T = core.DefaultTenant

var bg = context.Background()

const flowDoc = `flow: demo
version: 1.0.0
on:
%s
%s
steps:
  - id: a
    kind: exec
    exec: { argv: [echo, hi] }
`

type rig struct {
	f *coretest.Fixture
	s *triggers.Scheduler
	v core.FlowVersion
}

func newRig(t *testing.T, on, extra string) *rig {
	t.Helper()
	f := coretest.New()
	r := &rig{f: f}
	r.s = triggers.New(triggers.Deps{
		Clock: f.Clock, IDs: f.IDs, Tenants: f.DB.Tenants, Flows: f.DB.Flows, Triggers: f.DB.Triggers,
		Jobs: f.Jobs, Starter: f.Starter, RunLog: f.RunLog,
	})
	r.s.Register(f.Worker)
	r.v = r.put(t, on, extra, "h1")
	return r
}

func (r *rig) put(t *testing.T, on, extra, hash string) core.FlowVersion {
	t.Helper()
	_, _, err := r.f.DB.Flows.PutVersion(bg, core.NewFlowVersion{
		Tenant: T, Name: "demo", Version: "1.0.0", Hash: hash, Source: []byte(fmt.Sprintf(flowDoc, on, extra)),
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.f.DB.Flows.SetState(bg, T, "demo", hash, core.FlowActive, r.f.Clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (r *rig) sync(t *testing.T) {
	t.Helper()
	if err := r.s.Sync(bg, T, r.v); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) recs(t *testing.T) []core.TriggerRecord {
	t.Helper()
	rs, err := r.f.DB.Triggers.ListTriggers(bg, T, "demo")
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func (r *rig) runs(t *testing.T) []core.Run {
	t.Helper()
	rs, _ := r.f.DB.Runs.ListRuns(bg, T, core.RunFilter{})
	return rs
}

func (r *rig) topics() []bus.Topic { return r.f.RunLog.Topics(T, "") }

func (r *rig) count(topic bus.Topic) int {
	n := 0
	for _, tp := range r.topics() {
		if tp == topic {
			n++
		}
	}
	return n
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

const daily = `  - schedule: { rrule: "FREQ=DAILY;BYHOUR=7", tz: America/Toronto }`

func TestDailyAtSevenAcrossDST(t *testing.T) {
	r := newRig(t, daily, "")
	r.f.Clock.Set(utc("2027-03-12T15:00:00Z")) // Friday afternoon, EST; DST starts on the 14th
	r.sync(t)
	toronto, _ := time.LoadLocation("America/Toronto")
	want := []string{"2027-03-13T12:00:00Z", "2027-03-14T11:00:00Z", "2027-03-15T11:00:00Z"} // spring forward on the 14th
	for i, w := range want {
		pend := r.f.Jobs.Pending(core.JobTriggerFire)
		if len(pend) != 1 || !pend[0].ScheduledAt.Equal(utc(w)) {
			t.Fatalf("fire %d: pending %+v, want one at %s", i, pend, w)
		}
		if h := pend[0].ScheduledAt.In(toronto).Hour(); h != 7 {
			t.Errorf("fire %d is at local hour %d", i, h)
		}
		if rec := r.recs(t)[0]; rec.NextAt == nil || !rec.NextAt.Equal(utc(w)) || rec.NextJob != pend[0].ID {
			t.Errorf("fire %d: trigger next = %v %s", i, rec.NextAt, rec.NextJob)
		}
		r.f.Clock.Set(utc(w))
		r.f.Worker.Drain(bg)
		for _, run := range r.runs(t) { // each run finishes before the next fire, or the overlap rule skips it
			_, _ = r.f.DB.State.FinishRun(bg, T, run.ID, core.RunFinish{Status: core.RunSucceeded})
		}
	}
	runs := r.runs(t)
	if len(runs) != 3 {
		t.Fatalf("%d runs", len(runs))
	}
	for _, run := range runs {
		if run.Trigger.Kind != specrun.TriggerSchedule || run.IdemKey != "schedule:"+run.Trigger.At.UTC().Format(time.RFC3339) {
			t.Errorf("run = %+v", run)
		}
	}
	if len(r.f.Jobs.Pending(core.JobTriggerFire)) != 1 {
		t.Error("exactly one next job stands")
	}
}

func TestDoubleFireQueuesOneNextJobAndOneRun(t *testing.T) {
	r := newRig(t, daily, "")
	r.sync(t)
	var handler core.JobHandler
	h := captureHandlers{}
	r.s.Register(h)
	handler = h[core.JobTriggerFire]
	job := r.f.Jobs.Pending(core.JobTriggerFire)[0]
	r.f.Clock.Set(job.ScheduledAt)
	for i := 0; i < 2; i++ {
		if err := handler(bg, job.Job); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(r.f.Jobs.Pending(core.JobTriggerFire)); n != 2 { // the original (never claimed here) and exactly one next
		t.Errorf("%d pending fire jobs, want the original plus one next", n)
	}
	next := 0
	for _, j := range r.f.Jobs.Pending(core.JobTriggerFire) {
		if j.ScheduledAt.After(job.ScheduledAt) {
			next++
		}
	}
	if next != 1 {
		t.Errorf("%d next jobs", next)
	}
	if runs := r.runs(t); len(runs) != 1 {
		t.Errorf("%d runs for one occurrence", len(runs))
	}
	fires, _ := r.f.DB.Triggers.ListFires(bg, T, "demo", "", 0)
	if len(fires) != 2 || fires[0].Outcome != core.FireDuplicate || fires[1].Outcome != core.FireStarted {
		t.Errorf("fires = %+v", fires)
	}
}

type captureHandlers map[core.JobType]core.JobHandler

func (c captureHandlers) Handle(t core.JobType, h core.JobHandler) { c[t] = h }
func (c captureHandlers) Serve(context.Context) error              { return nil }

func TestMissedFireWithinPeriodRunsOnce(t *testing.T) {
	r := newRig(t, daily, "")
	r.f.Clock.Set(utc("2026-11-18T10:00:00Z")) // 05:00 EST, before 07:00
	r.sync(t)
	due := r.f.Jobs.Pending(core.JobTriggerFire)[0].ScheduledAt // 12:00Z
	r.f.Clock.Set(due.Add(5 * time.Hour))                       // engine was down; back at noon local
	r.f.Worker.Drain(bg)
	runs := r.runs(t)
	if len(runs) != 1 || !runs[0].Trigger.At.Equal(due) {
		t.Fatalf("runs = %+v", runs)
	}
	if r.count(core.TopicTriggerSkipped) != 0 {
		t.Error("a fire inside its period is not a skip")
	}
	if next := r.f.Jobs.Pending(core.JobTriggerFire); len(next) != 1 || !next[0].ScheduledAt.Equal(due.Add(24*time.Hour)) {
		t.Errorf("next = %+v", next)
	}
}

func TestMissedFiresPastPeriodAreDroppedAndRecorded(t *testing.T) {
	r := newRig(t, daily, "")
	r.f.Clock.Set(utc("2026-11-18T10:00:00Z"))
	r.sync(t)
	first := r.f.Jobs.Pending(core.JobTriggerFire)[0].ScheduledAt // Nov 18 12:00Z
	r.f.Clock.Set(first.Add(2*24*time.Hour + 2*time.Hour))        // Nov 20 14:00Z: occurrences 18 and 19 are over, 20 is current
	r.f.Worker.Drain(bg)
	runs := r.runs(t)
	if len(runs) != 1 || !runs[0].Trigger.At.Equal(first.Add(48*time.Hour)) {
		t.Fatalf("only the occurrence whose period is current runs: %+v", runs)
	}
	fires, _ := r.f.DB.Triggers.ListFires(bg, T, "demo", "", 0)
	var missed *core.Fire
	for i := range fires {
		if fires[i].Outcome == core.FireMissed {
			missed = &fires[i]
		}
	}
	if missed == nil || !missed.At.Equal(first.Add(24*time.Hour)) || missed.Run != "" {
		t.Fatalf("missed fire = %+v (all %+v)", missed, fires)
	}
	if r.count(core.TopicTriggerSkipped) != 1 {
		t.Errorf("trigger.skipped events = %d", r.count(core.TopicTriggerSkipped))
	}
	evs := r.f.RunLog.Events(T, "")
	var found bool
	for _, e := range evs {
		if e.Topic == core.TopicTriggerSkipped {
			p, _ := e.Payload.(core.TriggerEvent)
			found = p.Outcome == core.FireMissed && p.Reason != ""
		}
	}
	if !found {
		t.Error("the skip event carries outcome missed and a reason")
	}
	if next := r.f.Jobs.Pending(core.JobTriggerFire); len(next) != 1 || !next[0].ScheduledAt.Equal(first.Add(72*time.Hour)) {
		t.Errorf("next = %+v", next)
	}
}

func TestOverlapSkipsButKeepsTheChain(t *testing.T) {
	r := newRig(t, `  - schedule: { rrule: "FREQ=HOURLY" }`, "")
	r.sync(t)
	for i := 0; i < 3; i++ {
		due := r.f.Jobs.Pending(core.JobTriggerFire)[0].ScheduledAt
		r.f.Clock.Set(due)
		r.f.Worker.Drain(bg)
	}
	// the first run is still queued, never finished: default for schedule is skip
	if runs := r.runs(t); len(runs) != 1 {
		t.Fatalf("%d runs", len(runs))
	}
	fires, _ := r.f.DB.Triggers.ListFires(bg, T, "demo", "", 0)
	if len(fires) != 3 || fires[0].Outcome != core.FireSkipped || fires[0].Reason != "overlap" {
		t.Errorf("fires = %+v", fires)
	}
	if len(r.f.Jobs.Pending(core.JobTriggerFire)) != 1 {
		t.Error("the chain continues")
	}
}

func TestFlowNotActiveIsASkipNotAFailure(t *testing.T) {
	r := newRig(t, daily, "")
	r.sync(t)
	if _, err := r.f.DB.Flows.SetState(bg, T, "demo", "h1", core.FlowPaused, time.Time{}); err != nil {
		t.Fatal(err)
	}
	due := r.f.Jobs.Pending(core.JobTriggerFire)[0].ScheduledAt
	r.f.Clock.Set(due)
	r.f.Worker.Drain(bg)
	fires, _ := r.f.DB.Triggers.ListFires(bg, T, "demo", "", 0)
	if len(fires) != 1 || fires[0].Outcome != core.FireSkipped || fires[0].Reason != string(core.CodeFlowNotActive) {
		t.Errorf("fires = %+v", fires)
	}
	if len(r.f.Jobs.Pending(core.JobTriggerFire)) != 1 {
		t.Error("the chain continues")
	}
}

func TestSyncUnsyncAndVersionChange(t *testing.T) {
	r := newRig(t, daily+"\n  - manual: {}", "")
	r.sync(t)
	recs := r.recs(t)
	if len(recs) != 2 || recs[0].Spec.Kind != specrun.TriggerSchedule || recs[1].Spec.Kind != specrun.TriggerManual || recs[1].NextJob != "" {
		t.Fatalf("recs = %+v", recs)
	}
	job := recs[0].NextJob
	r.sync(t) // idempotent
	if again := r.recs(t); again[0].ID != recs[0].ID || again[0].NextJob != job || len(r.f.Jobs.Pending(core.JobTriggerFire)) != 1 {
		t.Errorf("sync is idempotent: %+v", again)
	}

	// a new version with the same declaration keeps registration and job
	v2 := r.put(t, daily+"\n  - manual: {}", "# v2", "h2")
	if err := r.s.Sync(bg, T, v2); err != nil {
		t.Fatal(err)
	}
	if kept := r.recs(t); kept[0].ID != recs[0].ID || kept[0].NextJob != job || kept[0].Hash != "h2" {
		t.Errorf("kept = %+v", kept[0])
	}

	// a version that drops the schedule removes it and cancels its job
	v3 := r.put(t, "  - manual: {}", "# v3", "h3")
	if err := r.s.Sync(bg, T, v3); err != nil {
		t.Fatal(err)
	}
	if left := r.recs(t); len(left) != 1 || left[0].Spec.Kind != specrun.TriggerManual {
		t.Errorf("left = %+v", left)
	}
	if info, _ := r.f.Jobs.Get(bg, core.JobID(job)); info.Status != core.JobCancelled {
		t.Errorf("dropped trigger's job = %s", info.Status)
	}
	if r.count(core.TopicTriggerCancelled) == 0 {
		t.Error("cancelled event")
	}

	// back to a schedule, then pause the flow
	v4 := r.put(t, daily, "# v4", "h4")
	_ = r.s.Sync(bg, T, v4)
	if len(r.f.Jobs.Pending(core.JobTriggerFire)) != 1 {
		t.Fatal("re-registered")
	}
	if err := r.s.Unsync(bg, T, "demo"); err != nil {
		t.Fatal(err)
	}
	if rec := r.recs(t)[0]; rec.State != core.TriggerPaused || rec.NextAt != nil || rec.NextJob != "" {
		t.Errorf("paused = %+v", rec)
	}
	if len(r.f.Jobs.Pending(core.JobTriggerFire)) != 0 {
		t.Error("unsync cancels the queued fire")
	}
	if err := r.s.Sync(bg, T, v4); err != nil {
		t.Fatal(err)
	}
	if rec := r.recs(t)[0]; rec.State != core.TriggerActive || rec.NextJob == "" || len(r.f.Jobs.Pending(core.JobTriggerFire)) != 1 {
		t.Errorf("reactivated = %+v", rec)
	}
}

func TestPausedOrRemovedTriggerFiresNothing(t *testing.T) {
	r := newRig(t, daily, "")
	r.sync(t)
	h := captureHandlers{}
	r.s.Register(h)
	job := r.f.Jobs.Pending(core.JobTriggerFire)[0]
	_ = r.s.Unsync(bg, T, "demo")
	r.f.Clock.Set(job.ScheduledAt)
	if err := h[core.JobTriggerFire](bg, job.Job); err != nil {
		t.Fatal(err)
	}
	if len(r.runs(t)) != 0 || len(r.f.Jobs.Pending(core.JobTriggerFire)) != 0 {
		t.Error("a paused trigger starts nothing and queues nothing")
	}
	_ = r.f.DB.Triggers.DeleteTriggers(bg, T, "demo")
	if err := h[core.JobTriggerFire](bg, job.Job); err != nil {
		t.Errorf("a removed trigger's stale job is dropped: %v", err)
	}
}

func TestJitterIsDeterministicAndBounded(t *testing.T) {
	r := newRig(t, `  - schedule: { rrule: "FREQ=DAILY;BYHOUR=7", tz: UTC, jitter: 10m }`, "")
	r.f.Clock.Set(utc("2026-11-19T00:00:00Z"))
	r.sync(t)
	j := r.f.Jobs.Pending(core.JobTriggerFire)[0]
	occ := utc("2026-11-19T07:00:00Z")
	if j.ScheduledAt.Before(occ) || !j.ScheduledAt.Before(occ.Add(10*time.Minute)) {
		t.Errorf("fires at %s, want within [07:00, 07:10)", j.ScheduledAt)
	}
	if rec := r.recs(t)[0]; !rec.NextAt.Equal(occ) {
		t.Errorf("the occurrence stays 07:00 for the run key: %v", rec.NextAt)
	}
	if err := r.s.Sync(bg, T, r.v); err != nil || len(r.f.Jobs.Pending(core.JobTriggerFire)) != 1 {
		t.Error("re-sync keeps the one job")
	}
}

func TestQuarterHourScheduleAlignsToTheDay(t *testing.T) {
	r := newRig(t, `  - schedule: { rrule: "FREQ=MINUTELY;INTERVAL=15" }`, "")
	r.f.Clock.Set(utc("2026-11-18T12:07:30Z"))
	r.sync(t)
	got := r.f.Jobs.Pending(core.JobTriggerFire)[0].ScheduledAt
	if !got.Equal(utc("2026-11-18T12:15:00Z")) {
		t.Errorf("first = %s", got)
	}
	r.f.Clock.Set(got)
	r.f.Worker.Drain(bg)
	if next := r.f.Jobs.Pending(core.JobTriggerFire)[0].ScheduledAt; !next.Equal(utc("2026-11-18T12:30:00Z")) {
		t.Errorf("next = %s", next)
	}
}

func TestReconcileRequeuesALostNextFireAndSkipsPausedFlows(t *testing.T) {
	r := newRig(t, daily, "")
	r.sync(t)
	rec := r.recs(t)[0]
	if err := r.f.Jobs.Cancel(bg, rec.NextJob); err != nil { // the queue lost it
		t.Fatal(err)
	}
	if err := r.s.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	if pend := r.f.Jobs.Pending(core.JobTriggerFire); len(pend) != 1 || pend[0].ID == rec.NextJob {
		t.Fatalf("pending = %+v", pend)
	}
	if got := r.recs(t)[0]; got.NextJob == rec.NextJob || got.NextJob == "" {
		t.Errorf("record still points at the lost job: %+v", got)
	}
	if err := r.s.Reconcile(bg); err != nil || len(r.f.Jobs.Pending(core.JobTriggerFire)) != 1 {
		t.Errorf("reconcile is idempotent: %v", err)
	}
	_, _ = r.f.DB.Flows.SetState(bg, T, "demo", "h1", core.FlowPaused, time.Time{})
	_ = r.s.Unsync(bg, T, "demo")
	_ = r.s.Reconcile(bg)
	if len(r.f.Jobs.Pending(core.JobTriggerFire)) != 0 {
		t.Error("a paused flow stays quiet")
	}
}

func TestSyncRefusesAnInvalidSchedule(t *testing.T) {
	r := newRig(t, `  - schedule: { rrule: "FREQ=SOMETIMES" }`, "")
	if err := r.s.Sync(bg, T, r.v); !core.IsCode(err, core.CodeInvalidFlow) {
		t.Errorf("err = %v", err)
	}
	r2 := newRig(t, `  - schedule: { rrule: "FREQ=DAILY", tz: Mars/Olympus }`, "")
	if err := r2.s.Sync(bg, T, r2.v); !core.IsCode(err, core.CodeInvalidFlow) {
		t.Errorf("tz err = %v", err)
	}
}
