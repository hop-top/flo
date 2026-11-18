package calendar_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/nodes/plugin"
	"hop.top/flo/internal/packs/calendar"
	"hop.top/flo/internal/triggers"
)

const T = core.DefaultTenant

const flowDoc = `flow: demo
version: 1.0.0
on:
  - calendar: { source: calendar/events@1, offset: -30m%s }
concurrency: { max: 20 }
steps:
  - id: a
    kind: exec
    exec: { argv: [echo, hi] }
`

type rig struct {
	f    *coretest.Fixture
	s    *calendar.Syncer
	file string
	v    core.FlowVersion
}

// newRig has the clock at 2026-11-18 12:00Z and an ICS file the test rewrites.
func newRig(t *testing.T, extra string) *rig {
	t.Helper()
	f := coretest.New()
	sched := triggers.New(triggers.Deps{
		Clock: f.Clock, IDs: f.IDs, Tenants: f.DB.Tenants, Flows: f.DB.Flows, Triggers: f.DB.Triggers,
		Jobs: f.Jobs, Starter: f.Starter, RunLog: f.RunLog,
	})
	sched.Register(f.Worker)
	reg := plugin.NewRegistry()
	if err := reg.RegisterTrigger(&calendar.Plugin{Clock: f.Clock}); err != nil {
		t.Fatal(err)
	}
	r := &rig{f: f, file: filepath.Join(t.TempDir(), "cal.ics")}
	r.s = calendar.NewSyncer(calendar.Deps{
		Clock: f.Clock, Tenants: f.DB.Tenants, Triggers: f.DB.Triggers, Jobs: f.Jobs, Plugins: reg, Scheduler: sched,
		Config: map[string]any{"url": r.file},
	})
	r.s.Register(f.Worker)
	if _, _, err := f.DB.Flows.PutVersion(bg, core.NewFlowVersion{
		Tenant: T, Name: "demo", Version: "1.0.0", Hash: "h1", Source: []byte(fmt.Sprintf(flowDoc, extra)),
	}); err != nil {
		t.Fatal(err)
	}
	v, err := f.DB.Flows.SetState(bg, T, "demo", "h1", core.FlowActive, f.Clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	r.v = v
	if err := calendar.NewSyncer(r.s.Deps).Hook(sched).Sync(bg, T, v); err != nil { // activation
		t.Fatal(err)
	}
	return r
}

func (r *rig) write(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(r.file, ics(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) rec(t *testing.T) core.TriggerRecord {
	t.Helper()
	rs, err := r.f.DB.Triggers.ListTriggers(bg, T, "demo")
	if err != nil || len(rs) != 1 {
		t.Fatalf("triggers %v %v", rs, err)
	}
	return rs[0]
}

func (r *rig) sync(t *testing.T) triggers.PlanResult {
	t.Helper()
	res, err := r.s.SyncTrigger(bg, r.rec(t))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (r *rig) fires() []core.JobInfo { return r.f.Jobs.Pending(core.JobCalendarFire) }

func (r *rig) runs(t *testing.T) []core.Run {
	t.Helper()
	rs, err := r.f.DB.Runs.ListRuns(bg, T, core.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func event(uid, start, extra string) string {
	return fmt.Sprintf("BEGIN:VEVENT\nUID:%s\nDTSTART:%s\nSUMMARY:%s\n%sEND:VEVENT\n", uid, start, uid, extra)
}

func TestStartMinusThirtyMinutes(t *testing.T) {
	r := newRig(t, "")
	r.write(t, event("m", "20261118T150000Z", ""))
	if res := r.sync(t); len(res.Planned) != 1 {
		t.Fatalf("plan = %+v", res)
	}
	pend := r.fires()
	if len(pend) != 1 || !pend[0].ScheduledAt.Equal(utc("2026-11-18T14:30:00Z")) {
		t.Fatalf("fires = %+v", pend)
	}
}

func TestMovedEventCancelsOldJobAndPlansNew(t *testing.T) {
	r := newRig(t, "")
	r.write(t, event("m", "20261118T150000Z", ""))
	r.sync(t)
	old := r.fires()[0]
	r.write(t, event("m", "20261118T170000Z", ""))
	res := r.sync(t)
	if len(res.Moved) != 1 {
		t.Fatalf("plan = %+v", res)
	}
	if info, _ := r.f.Jobs.Get(bg, old.ID); info.Status != core.JobCancelled {
		t.Errorf("old job = %s", info.Status)
	}
	pend := r.fires()
	if len(pend) != 1 || pend[0].ID == old.ID || !pend[0].ScheduledAt.Equal(utc("2026-11-18T16:30:00Z")) {
		t.Fatalf("fires = %+v", pend)
	}
}

func TestCancelledEventHasNoJob(t *testing.T) {
	r := newRig(t, "")
	r.write(t, event("m", "20261118T150000Z", ""))
	r.sync(t)
	r.write(t, event("m", "20261118T150000Z", "STATUS:CANCELLED\n"))
	if res := r.sync(t); len(res.Cancelled) != 1 {
		t.Fatalf("plan = %+v", res)
	}
	if pend := r.fires(); len(pend) != 0 {
		t.Fatalf("fires = %+v", pend)
	}
	// and one that is cancelled before the first sync is never planned
	r.write(t, event("c", "20261118T160000Z", "STATUS:CANCELLED\n"))
	r.sync(t)
	if pend := r.fires(); len(pend) != 0 {
		t.Fatalf("fires = %+v", pend)
	}
}

func TestPastFireTimeFiresImmediately(t *testing.T) {
	r := newRig(t, "")
	r.write(t, event("soon", "20261118T122000Z", "")) // starts in 20m; fire time was 10m ago
	r.sync(t)
	pend := r.fires()
	if len(pend) != 1 || !pend[0].ScheduledAt.Equal(r.f.Clock.Now()) {
		t.Fatalf("fires = %+v", pend)
	}
	r.f.Worker.Drain(bg)
	runs := r.runs(t)
	if len(runs) != 1 || runs[0].Trigger.Payload["event"].(map[string]any)["uid"] != "soon" {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestRecurringEventExpandsWithinHorizonAcrossDST(t *testing.T) {
	r := newRig(t, "")
	r.f.Clock.Set(utc("2027-03-25T12:00:00Z"))
	r.write(t, `BEGIN:VEVENT
UID:daily
DTSTART;TZID=Europe/Paris:20270301T090000
RRULE:FREQ=DAILY
SUMMARY:Standup
END:VEVENT
`)
	r.sync(t)
	var got []string
	for _, j := range r.fires() {
		got = append(got, j.ScheduledAt.Format(time.RFC3339))
	}
	// horizon 7d from the 25th 12:00Z: fires for the starts of the 26th..; the DST change is on the 28th
	want := []string{
		"2027-03-26T07:30:00Z", "2027-03-27T07:30:00Z", "2027-03-28T06:30:00Z", "2027-03-29T06:30:00Z",
		"2027-03-30T06:30:00Z", "2027-03-31T06:30:00Z", "2027-04-01T06:30:00Z",
	}
	eq(t, got, want)
}

func TestSyncIsIdempotent(t *testing.T) {
	r := newRig(t, "")
	r.write(t, event("a", "20261118T150000Z", "")+event("b", "20261119T150000Z", ""))
	r.sync(t)
	before := r.fires()
	res := r.sync(t)
	if len(res.Kept) != 2 || len(res.Planned)+len(res.Moved)+len(res.Cancelled) != 0 {
		t.Fatalf("second sync = %+v", res)
	}
	after := r.fires()
	if len(after) != 2 || after[0].ID != before[0].ID || after[1].ID != before[1].ID {
		t.Fatalf("jobs changed: %+v -> %+v", before, after)
	}
}

func TestWhenIsJudgedAtPlanTime(t *testing.T) {
	r := newRig(t, `, when: "trigger.event.attendees.size() > 1"`)
	r.write(t, event("solo", "20261118T150000Z", "ATTENDEE:mailto:a@x\n")+
		event("team", "20261118T160000Z", "ATTENDEE:mailto:a@x\nATTENDEE:mailto:b@x\n")+
		event("none", "20261118T170000Z", ""))
	r.sync(t)
	pend := r.fires()
	if len(pend) != 1 || !pend[0].ScheduledAt.Equal(utc("2026-11-18T15:30:00Z")) {
		t.Fatalf("fires = %+v", pend)
	}
}

func TestWhenFailingOnOneEventIsReportedAndSkipsOnlyIt(t *testing.T) {
	r := newRig(t, `, when: "trigger.event.location != '' || trigger.event.nope == 1"`)
	r.write(t, event("ok", "20261118T150000Z", "LOCATION:Room\n")+event("bad", "20261118T160000Z", ""))
	if _, err := r.s.SyncTrigger(bg, r.rec(t)); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("err = %v", err)
	}
	if pend := r.fires(); len(pend) != 1 || !pend[0].ScheduledAt.Equal(utc("2026-11-18T14:30:00Z")) {
		t.Fatalf("fires = %+v", pend)
	}
}

func TestSourceFailureKeepsPlannedFires(t *testing.T) {
	r := newRig(t, "")
	r.write(t, event("a", "20261118T150000Z", ""))
	r.sync(t)
	if err := os.Remove(r.file); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.SyncTrigger(bg, r.rec(t)); err == nil {
		t.Fatal("a missing calendar is an error, not an empty calendar")
	}
	if pend := r.fires(); len(pend) != 1 {
		t.Fatalf("fires = %+v", pend)
	}
}

// End to end on the job worker: activation starts the chain, each event
// fires once at start minus 30m, a re-sync after a fire does not fire it
// again, and the chain keeps exactly one next sync queued.
func TestCalendarFlowFiresOncePerEvent(t *testing.T) {
	r := newRig(t, "")
	r.write(t, event("a", "20261118T150000Z", "")+event("b", "20261118T170000Z", "")+event("c", "20261125T150000Z", ""))
	r.f.Worker.Drain(bg) // activation queued the first sync
	if pend := r.fires(); len(pend) != 2 {
		t.Fatalf("after first sync: %+v (the third event is beyond the horizon)", pend)
	}
	syncs := func() []core.JobInfo { return r.f.Jobs.Pending(core.JobCalendarSync) }
	if got := syncs(); len(got) != 1 || !got[0].ScheduledAt.Equal(r.f.Clock.Now().Add(r.s.Interval())) {
		t.Fatalf("sync chain = %+v", got)
	}
	r.f.Clock.Set(utc("2026-11-18T14:30:00Z"))
	r.f.Worker.Drain(bg) // fire a; the sync due at 12:10 also ran and re-planned
	if runs := r.runs(t); len(runs) != 1 {
		t.Fatalf("runs after a = %d", len(runs))
	}
	r.f.Clock.Set(utc("2026-11-18T16:30:00Z"))
	r.f.Worker.Drain(bg)
	runs := r.runs(t)
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want one per event", len(runs))
	}
	seen := map[string]bool{}
	for _, run := range runs {
		seen[run.Trigger.Payload["event"].(map[string]any)["uid"].(string)] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("events run: %v", seen)
	}
	// once more over the same file: nothing fires again
	r.f.Clock.Set(utc("2026-11-18T16:45:00Z"))
	r.f.Worker.Drain(bg)
	if n := len(r.runs(t)); n != 2 {
		t.Errorf("runs after more syncs = %d", n)
	}
	if got := syncs(); len(got) != 1 {
		t.Errorf("sync chain = %+v", got)
	}
}

func TestPastFireIsNotRefiredByLaterSyncs(t *testing.T) {
	r := newRig(t, "")
	r.write(t, event("soon", "20261118T122000Z", ""))
	r.f.Worker.Drain(bg) // sync plans an immediate fire, the worker runs it
	if n := len(r.runs(t)); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	r.f.Clock.Set(utc("2026-11-18T12:10:00Z")) // next sync; the event has not started
	r.f.Worker.Drain(bg)
	if pend := r.fires(); len(pend) != 0 {
		t.Fatalf("a fired event is planned again: %+v", pend)
	}
	if n := len(r.runs(t)); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	if fs, _ := r.f.DB.Triggers.ListFires(bg, T, "demo", r.rec(t).ID, 0); len(fs) != 1 || fs[0].Outcome != core.FireStarted {
		t.Fatalf("fires recorded = %+v, want the one start", fs)
	}
}

func TestEnsureStartsOneChainAndPauseEndsIt(t *testing.T) {
	r := newRig(t, "")
	rec := r.rec(t)
	for i := 0; i < 3; i++ {
		if err := r.s.Ensure(bg, rec); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.f.Jobs.Pending(core.JobCalendarSync); len(got) != 1 {
		t.Fatalf("chains = %+v", got)
	}
	if err := r.s.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	if got := r.f.Jobs.Pending(core.JobCalendarSync); len(got) != 1 {
		t.Fatalf("reconcile doubled the chain: %+v", got)
	}
	// pause: the sync that runs next ends the chain
	rec.State = core.TriggerPaused
	if err := r.f.DB.Triggers.PutTrigger(bg, rec); err != nil {
		t.Fatal(err)
	}
	r.f.Worker.Drain(bg)
	r.f.Clock.Set(r.f.Clock.Now().Add(time.Hour))
	r.f.Worker.Drain(bg)
	if got := r.f.Jobs.Pending(core.JobCalendarSync); len(got) != 0 {
		t.Fatalf("paused trigger still syncs: %+v", got)
	}
}

func TestSyncIntervalFromConfig(t *testing.T) {
	r := newRig(t, "")
	d := r.s.Deps
	for in, want := range map[any]time.Duration{nil: calendar.DefaultSyncInterval, "2m": 2 * time.Minute, "junk": calendar.DefaultSyncInterval, "-1m": calendar.DefaultSyncInterval} {
		d.Config = map[string]any{"sync_interval": in}
		if got := calendar.NewSyncer(d).Interval(); got != want {
			t.Errorf("%v: %s", in, got)
		}
	}
	_ = strings.TrimSpace
}
