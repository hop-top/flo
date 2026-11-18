package triggers_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/jobs"
	"hop.top/flo/internal/runlog"
	"hop.top/flo/internal/store/sqlite"
	"hop.top/flo/internal/triggers"
)

// The schedule runs end to end on the real stack: SQLite stores, the
// durabletask queue and worker, the file run log.
func TestScheduleOnTheRealStack(t *testing.T) {
	root := t.TempDir()
	clock := coretest.NewClock(time.Time{})
	clock.Set(utc("2027-03-12T15:00:00Z"))
	ids := &coretest.IDs{}
	st := sqlite.Open(root, sqlite.Options{Clock: clock, NoSync: true})
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Tenants.Ensure(bg, T); err != nil {
		t.Fatal(err)
	}
	q, err := jobs.Open(filepath.Join(root, "jobs.db"), jobs.Options{Clock: clock, NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	rl := runlog.New(runlog.Options{RunsDir: st.Registry().RunsDir})
	starter := core.NewStarter(core.StarterDeps{Clock: clock, IDs: ids, Flows: st.Flows, Runs: st.Runs, Groups: st.Groups, Jobs: q, RunLog: rl})
	s := triggers.New(triggers.Deps{
		Clock: clock, IDs: ids, Tenants: st.Tenants, Flows: st.Flows, Triggers: st.Triggers, Jobs: q, Starter: starter, RunLog: rl,
	})
	w := jobs.NewWorker(q, jobs.WorkerOptions{Pollers: 2, Interval: 5 * time.Millisecond})
	s.Register(w)

	v, _, err := st.Flows.PutVersion(bg, core.NewFlowVersion{Tenant: T, Name: "demo", Version: "1.0.0", Hash: "h1", Source: []byte(fmt.Sprintf(flowDoc, daily, ""))})
	if err != nil {
		t.Fatal(err)
	}
	if v, err = st.Flows.SetState(bg, T, "demo", "h1", core.FlowActive, clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(bg, T, v); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { _ = w.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	pending := func() []core.JobInfo {
		l, _ := q.List(bg, core.JobFilter{Type: core.JobTriggerFire, Statuses: []core.JobStatus{core.JobPending, core.JobActive}})
		return l
	}
	for _, due := range []string{"2027-03-13T12:00:00Z", "2027-03-14T11:00:00Z", "2027-03-15T11:00:00Z"} {
		p := pending()
		if len(p) != 1 || !p[0].ScheduledAt.Equal(utc(due)) {
			t.Fatalf("pending %+v, want one at %s", p, due)
		}
		clock.Set(utc(due))
		deadline := time.Now().Add(10 * time.Second)
		for {
			runs, _ := st.Runs.ListRuns(bg, T, core.RunFilter{})
			next := pending()
			if len(runs) > 0 && runs[0].Trigger.At.Equal(utc(due)) && len(next) == 1 && next[0].ScheduledAt.After(utc(due)) {
				_, _ = st.State.FinishRun(bg, T, runs[0].ID, core.RunFinish{Status: core.RunSucceeded})
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no fire at %s: runs %d pending %+v", due, len(runs), next)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	runs, _ := st.Runs.ListRuns(bg, T, core.RunFilter{})
	if len(runs) != 3 {
		t.Errorf("%d runs", len(runs))
	}
	r, _ := rl.Open(bg, T, "", false)
	n := 0
	for {
		if _, err := r.Next(bg); err != nil {
			break
		}
		n++
	}
	if n < 6 { // scheduled + fired for each of three
		t.Errorf("engine log has %d events", n)
	}
}
