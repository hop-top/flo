package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/jobs"
)

const T = core.DefaultTenant

var bg = context.Background()

func openQueue(t *testing.T, path string, clock core.Clock, ttl time.Duration) *jobs.Queue {
	t.Helper()
	q, err := jobs.Open(path, jobs.Options{Clock: clock, ClaimTTL: ttl, NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func newQueue(t *testing.T, clock core.Clock) *jobs.Queue {
	t.Helper()
	return openQueue(t, filepath.Join(t.TempDir(), "jobs.db"), clock, 0)
}

// The Jobs contract runs on the fake queue and on kit's durabletask.
func bothQueues(t *testing.T, fn func(t *testing.T, q core.Jobs, clock *coretest.Clock)) {
	t.Helper()
	t.Run("fake", func(t *testing.T) {
		clock := coretest.NewClock(time.Time{})
		fn(t, coretest.NewJobs(clock, &coretest.IDs{}), clock)
	})
	t.Run("durabletask", func(t *testing.T) {
		clock := coretest.NewClock(time.Time{})
		fn(t, newQueue(t, clock), clock)
	})
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestEnqueueDedupesByTypeTenantKey(t *testing.T) {
	bothQueues(t, func(t *testing.T, q core.Jobs, _ *coretest.Clock) {
		a, err := q.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "r1", Payload: map[string]int{"n": 1}})
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "r1", Payload: map[string]int{"n": 2}}); a != b {
			t.Errorf("same type+tenant+key: %s vs %s", a, b)
		}
		for name, spec := range map[string]core.JobSpec{
			"other tenant": {Type: core.JobRunAdvance, Tenant: "other", Key: "r1"},
			"other type":   {Type: core.JobTriggerFire, Tenant: T, Key: "r1"},
			"other key":    {Type: core.JobRunAdvance, Tenant: T, Key: "r2"},
		} {
			if id, _ := q.Enqueue(bg, spec); id == a {
				t.Errorf("%s must not dedupe", name)
			}
		}
		x, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobApprovalPoll, Tenant: T})
		if y, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobApprovalPoll, Tenant: T}); x == y {
			t.Error("an empty key never dedupes")
		}
		info, err := q.Get(bg, a)
		if err != nil || info.Status != core.JobPending || info.Key != "r1" || info.Tenant != T || info.Type != core.JobRunAdvance {
			t.Fatalf("get = %+v %v", info, err)
		}
		var p struct{ N int }
		_ = json.Unmarshal(info.Payload, &p)
		if p.N != 1 {
			t.Errorf("the first payload wins: %s", info.Payload)
		}
		if _, err := q.Get(bg, "job_nope"); !core.IsCode(err, core.CodeJobNotFound) {
			t.Errorf("unknown job: %v", err)
		}
	})
}

func TestCancelFreesTheKeyAndIsIdempotent(t *testing.T) {
	bothQueues(t, func(t *testing.T, q core.Jobs, _ *coretest.Clock) {
		a, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobCalendarFire, Tenant: T, Key: "k"})
		if err := q.Cancel(bg, a); err != nil {
			t.Fatal(err)
		}
		if err := q.Cancel(bg, a); err != nil {
			t.Errorf("cancel a cancelled job: %v", err)
		}
		if err := q.Cancel(bg, "job_unknown"); err != nil {
			t.Errorf("cancel unknown: %v", err)
		}
		if info, _ := q.Get(bg, a); info.Status != core.JobCancelled {
			t.Errorf("status = %s", info.Status)
		}
		b, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobCalendarFire, Tenant: T, Key: "k"})
		if b == a {
			t.Error("a finished job no longer holds the key")
		}
	})
}

func TestListFiltersOldestFirst(t *testing.T) {
	bothQueues(t, func(t *testing.T, q core.Jobs, clock *coretest.Clock) {
		var ids []core.JobID
		for _, k := range []string{"a", "b", "c"} {
			clock.Advance(time.Second)
			id, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: k})
			ids = append(ids, id)
		}
		_, _ = q.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: "other", Key: "a"})
		_, _ = q.Enqueue(bg, core.JobSpec{Type: core.JobTriggerFire, Tenant: T, Key: "a"})
		_ = q.Cancel(bg, ids[1])
		all, _ := q.List(bg, core.JobFilter{Type: core.JobRunAdvance, Tenant: T})
		if len(all) != 3 || all[0].ID != ids[0] || all[2].ID != ids[2] {
			t.Errorf("oldest first: %+v", all)
		}
		live, _ := q.List(bg, core.JobFilter{Type: core.JobRunAdvance, Tenant: T, Statuses: []core.JobStatus{core.JobPending, core.JobActive}})
		if len(live) != 2 {
			t.Errorf("unfinished: %d", len(live))
		}
		if one, _ := q.List(bg, core.JobFilter{Type: core.JobRunAdvance, Tenant: T, Key: "c"}); len(one) != 1 || one[0].ID != ids[2] {
			t.Errorf("by key: %+v", one)
		}
		if c, _ := q.List(bg, core.JobFilter{Statuses: []core.JobStatus{core.JobCancelled}}); len(c) != 1 {
			t.Errorf("cancelled: %d", len(c))
		}
	})
}

type served struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func serve(t *testing.T, w core.JobWorker) *served {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	s := &served{cancel: cancel, done: make(chan struct{})}
	go func() { _ = w.Serve(ctx); close(s.done) }()
	t.Cleanup(func() { cancel(); <-s.done })
	return s
}

func fastWorker(q *jobs.Queue, n int) *jobs.Worker {
	return jobs.NewWorker(q, jobs.WorkerOptions{Pollers: n, Interval: 5 * time.Millisecond, StaleInterval: 20 * time.Millisecond})
}

func TestScheduledAtDelaysClaim(t *testing.T) {
	clock := coretest.NewClock(time.Time{})
	q := newQueue(t, clock)
	var ran atomic.Int32
	w := fastWorker(q, 2)
	w.Handle(core.JobTriggerFire, func(context.Context, core.Job) error { ran.Add(1); return nil })
	at := clock.Now().Add(time.Hour)
	id, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobTriggerFire, Tenant: T, Key: "k", At: &at})
	serve(t, w)
	time.Sleep(60 * time.Millisecond)
	if ran.Load() != 0 {
		t.Fatal("ran before its time")
	}
	clock.Advance(time.Hour)
	eventually(t, "job success", func() bool { i, _ := q.Get(bg, id); return i.Status == core.JobSucceeded })
	if ran.Load() != 1 {
		t.Errorf("ran %d times", ran.Load())
	}
}

func TestHandlerNilFinishesErrorRetriesThenFails(t *testing.T) {
	clock := coretest.NewClock(time.Time{})
	q := openQueue(t, filepath.Join(t.TempDir(), "jobs.db"), clock, 0)
	var calls atomic.Int32
	w := fastWorker(q, 1)
	w.Handle(core.JobRunAdvance, func(_ context.Context, j core.Job) error {
		calls.Add(1)
		if j.Key == "ok" {
			return nil
		}
		return errors.New("boom")
	})
	w.Handle(core.JobTriggerFire, func(context.Context, core.Job) error { panic("handler bug") })
	ok, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "ok"})
	bad, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "bad", MaxAttempts: 2})
	pan, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobTriggerFire, Tenant: T, Key: "p", MaxAttempts: 1})
	serve(t, w)
	status := func(id core.JobID) core.JobStatus { i, _ := q.Get(bg, id); return i.Status }
	eventually(t, "ok", func() bool { return status(ok) == core.JobSucceeded })
	eventually(t, "first failure requeued", func() bool { i, _ := q.Get(bg, bad); return i.Status == core.JobPending && i.Attempt == 1 })
	eventually(t, "panic is a failed job", func() bool { return status(pan) == core.JobFailed })
	clock.Advance(time.Hour) // past the backoff
	eventually(t, "second failure is final", func() bool { return status(bad) == core.JobFailed })
	if i, _ := q.Get(bg, bad); i.Error != "boom" || i.Attempt != 2 {
		t.Errorf("bad = %+v", i)
	}
}

func TestPollersOwnWorkerIDs(t *testing.T) {
	q := newQueue(t, nil)
	const n = 4
	var mu sync.Mutex
	var running int
	release := make(chan struct{})
	w := jobs.NewWorker(q, jobs.WorkerOptions{Pollers: n, Interval: 5 * time.Millisecond, ID: "w"})
	w.Handle(core.JobRunAdvance, func(context.Context, core.Job) error {
		mu.Lock()
		running++
		mu.Unlock()
		<-release
		return nil
	})
	var ids []core.JobID
	for i := 0; i < n; i++ {
		id, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: string(rune('a' + i))})
		ids = append(ids, id)
	}
	s := serve(t, w)
	eventually(t, "all pollers busy", func() bool { mu.Lock(); defer mu.Unlock(); return running == n })
	owners := map[string]bool{}
	for _, id := range ids {
		j, err := jobs.ClaimedBy(q, id)
		if err != nil {
			t.Fatal(err)
		}
		owners[j] = true
	}
	if len(owners) != n {
		t.Errorf("claimed by %v: each poller needs its own WorkerID", owners)
	}
	close(release)
	s.cancel()
	<-s.done
}

func TestLongHandlerKeepsItsClaim(t *testing.T) {
	q := openQueue(t, filepath.Join(t.TempDir(), "jobs.db"), core.SystemClock{}, 150*time.Millisecond)
	started, finish := make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	w := fastWorker(q, 2)
	w.Handle(core.JobRunAdvance, func(context.Context, core.Job) error {
		runs.Add(1)
		close(started)
		<-finish
		return nil
	})
	id, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "long"})
	serve(t, w)
	var once sync.Once
	release := func() { once.Do(func() { close(finish) }) }
	t.Cleanup(release) // runs before serve's, so a failed test cannot hang the drain
	<-started
	deadline := time.Now().Add(700 * time.Millisecond) // > 4 claim TTLs
	for time.Now().Before(deadline) {
		if n, err := q.ReleaseStaleClaims(bg); err != nil || n != 0 {
			t.Fatalf("claim was released while the handler ran: %d %v", n, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if i, _ := q.Get(bg, id); i.Status != core.JobActive {
		t.Fatalf("status = %s", i.Status)
	}
	release()
	eventually(t, "success", func() bool { i, _ := q.Get(bg, id); return i.Status == core.JobSucceeded })
	if runs.Load() != 1 {
		t.Errorf("handler ran %d times", runs.Load())
	}
}

func TestKilledWorkerJobReturnsAfterReleaseStaleClaims(t *testing.T) {
	clock := coretest.NewClock(time.Time{})
	q := openQueue(t, filepath.Join(t.TempDir(), "jobs.db"), clock, time.Minute)
	id, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "r"})
	if err := jobs.ClaimAs(q, "dead-worker"); err != nil { // claims, never settles
		t.Fatal(err)
	}
	if i, _ := q.Get(bg, id); i.Status != core.JobActive {
		t.Fatalf("claimed = %s", i.Status)
	}
	clock.Advance(30 * time.Second)
	if n, _ := q.ReleaseStaleClaims(bg); n != 0 {
		t.Error("claim is still live")
	}
	clock.Advance(31 * time.Second)
	if n, err := q.ReleaseStaleClaims(bg); n != 1 || err != nil {
		t.Fatalf("released %d %v", n, err)
	}
	if i, _ := q.Get(bg, id); i.Status != core.JobPending {
		t.Fatalf("after release = %s", i.Status)
	}
	var ran atomic.Int32
	w := fastWorker(q, 1)
	w.Handle(core.JobRunAdvance, func(context.Context, core.Job) error { ran.Add(1); return nil })
	serve(t, w)
	eventually(t, "second life", func() bool { i, _ := q.Get(bg, id); return i.Status == core.JobSucceeded })
	if i, _ := q.Get(bg, id); i.Attempt != 2 {
		t.Errorf("attempt = %d", i.Attempt)
	}
}

func TestServeDrainsRunningHandlers(t *testing.T) {
	q := newQueue(t, nil)
	started, finish := make(chan struct{}), make(chan struct{})
	var ctxErr atomic.Value
	w := fastWorker(q, 1)
	w.Handle(core.JobRunAdvance, func(ctx context.Context, _ core.Job) error {
		close(started)
		<-finish
		ctxErr.Store(ctx.Err() == nil)
		return nil
	})
	id, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "r"})
	s := serve(t, w)
	<-started
	s.cancel()
	select {
	case <-s.done:
		t.Fatal("Serve returned with a handler still running")
	case <-time.After(100 * time.Millisecond):
	}
	// a job enqueued during the drain is not claimed
	late, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "late"})
	close(finish)
	<-s.done
	if i, _ := q.Get(bg, id); i.Status != core.JobSucceeded {
		t.Errorf("drained job = %s", i.Status)
	}
	if ok, _ := ctxErr.Load().(bool); !ok {
		t.Error("the handler's context stays live while it drains")
	}
	if i, _ := q.Get(bg, late); i.Status != core.JobPending {
		t.Errorf("late job = %s", i.Status)
	}
}

func TestShutdownGraceCancelsHandlers(t *testing.T) {
	q := newQueue(t, nil)
	started := make(chan struct{})
	w := jobs.NewWorker(q, jobs.WorkerOptions{Pollers: 1, Interval: 5 * time.Millisecond, ShutdownGrace: 50 * time.Millisecond})
	w.Handle(core.JobRunAdvance, func(ctx context.Context, _ core.Job) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	id, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "r"})
	s := serve(t, w)
	<-started
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the grace")
	}
	if i, _ := q.Get(bg, id); i.Status != core.JobPending || i.Error == "" {
		t.Errorf("interrupted job goes back to the queue: %+v", i)
	}
}

func TestConcurrentEnqueueAcrossQueuesIsOneJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	var qs []*jobs.Queue
	for i := 0; i < 5; i++ {
		qs = append(qs, openQueue(t, path, nil, 0))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[core.JobID]bool{}
	for r := 0; r < 5; r++ {
		for _, q := range qs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id, err := q.Enqueue(bg, core.JobSpec{Type: core.JobTriggerFire, Tenant: T, Key: "trg|2026-01-01T07:00:00Z"})
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				got[id] = true
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	if len(got) != 1 {
		t.Errorf("%d distinct jobs for one key: %v", len(got), got)
	}
	all, _ := qs[0].List(bg, core.JobFilter{Type: core.JobTriggerFire})
	if len(all) != 1 {
		t.Errorf("%d rows", len(all))
	}
}

func TestPayloadAndMetadataSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	clock := coretest.NewClock(time.Time{})
	q, err := jobs.Open(path, jobs.Options{Clock: clock, NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	at := clock.Now().Add(time.Hour)
	id, _ := q.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "r", At: &at, Payload: core.RunAdvancePayload{Tenant: T, Run: "run_1"}})
	_ = q.Close()
	q2 := openQueue(t, path, clock, 0)
	info, err := q2.Get(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	var p core.RunAdvancePayload
	_ = json.Unmarshal(info.Payload, &p)
	if p.Run != "run_1" || info.Tenant != T || !info.ScheduledAt.Equal(at) {
		t.Errorf("info = %+v payload %+v", info, p)
	}
	if again, _ := q2.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "r"}); again != id {
		t.Error("the key survives a reopen")
	}
}

// An enqueue that lands while the job runs is not dropped: the job runs again
// once the current run settles. A pending duplicate still dedupes.
func TestEnqueueDuringActiveRunsAgain(t *testing.T) {
	q := newQueue(t, nil)
	started, release := make(chan struct{}, 4), make(chan struct{})
	var runs atomic.Int32
	w := fastWorker(q, 3) // several pollers: the rerun must not overlap the first run
	var inFlight, overlapped atomic.Int32
	w.Handle(core.JobRunAdvance, func(context.Context, core.Job) error {
		if inFlight.Add(1) > 1 {
			overlapped.Add(1)
		}
		defer inFlight.Add(-1)
		n := runs.Add(1)
		started <- struct{}{}
		if n == 1 {
			<-release
		}
		return nil
	})
	spec := core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "r1"}
	first, _ := q.Enqueue(bg, spec)
	if dup, _ := q.Enqueue(bg, spec); dup != first {
		t.Fatal("a pending duplicate dedupes")
	}
	s := serve(t, w)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	<-started
	mid, _ := q.Enqueue(bg, spec) // mid-turn: the first run is active
	if mid != first {
		t.Errorf("returns the running job's id: %s vs %s", mid, first)
	}
	if again, _ := q.Enqueue(bg, spec); again != first {
		t.Error("repeat requests during the run collapse into one")
	}
	time.Sleep(60 * time.Millisecond)
	if runs.Load() != 1 {
		t.Fatal("the rerun must wait for the current run")
	}
	once.Do(func() { close(release) })
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the mid-turn enqueue was lost: no second run")
	}
	eventually(t, "second run settles", func() bool {
		l, _ := q.List(bg, core.JobFilter{Type: core.JobRunAdvance, Key: "r1", Statuses: []core.JobStatus{core.JobPending, core.JobActive}})
		return len(l) == 0 && runs.Load() == 2
	})
	time.Sleep(60 * time.Millisecond)
	if runs.Load() != 2 || overlapped.Load() != 0 {
		t.Errorf("ran %d times, %d overlapped; want 2 and 0", runs.Load(), overlapped.Load())
	}
	s.cancel()
	<-s.done
}

// The enqueue-while-active rule is part of the Jobs contract: it holds on the
// fake queue and on durabletask, the handler asking for itself mid-turn.
func TestContractEnqueueWhileActiveRerunsOnce(t *testing.T) {
	spec := core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "r1", Payload: map[string]int{"n": 1}}
	check := func(t *testing.T, q core.Jobs, w core.JobWorker, settle func(runs *atomic.Int32)) {
		var runs atomic.Int32
		var midIDs []core.JobID
		var firstID core.JobID
		w.Handle(core.JobRunAdvance, func(ctx context.Context, j core.Job) error {
			if runs.Add(1) == 1 {
				firstID = j.ID
				for i := 0; i < 3; i++ { // mid-turn: the job is active
					id, err := q.Enqueue(ctx, spec)
					if err != nil {
						return err
					}
					midIDs = append(midIDs, id)
				}
			}
			return nil
		})
		first, _ := q.Enqueue(bg, spec)
		if dup, _ := q.Enqueue(bg, spec); dup != first {
			t.Fatal("a pending duplicate dedupes")
		}
		settle(&runs)
		if firstID != first {
			t.Fatalf("handler saw %s, want %s", firstID, first)
		}
		for _, id := range midIDs {
			if id != first {
				t.Errorf("a mid-turn enqueue returns the running job's id: %s vs %s", id, first)
			}
		}
		if n := runs.Load(); n != 2 {
			t.Errorf("ran %d times; want the run and one rerun", n)
		}
		live, _ := q.List(bg, core.JobFilter{Type: core.JobRunAdvance, Key: "r1", Statuses: []core.JobStatus{core.JobPending, core.JobActive}})
		if len(live) != 0 {
			t.Errorf("%d jobs left unfinished", len(live))
		}
	}
	t.Run("fake", func(t *testing.T) {
		clock := coretest.NewClock(time.Time{})
		q := coretest.NewJobs(clock, &coretest.IDs{})
		w := coretest.NewWorker(q)
		check(t, q, w, func(*atomic.Int32) { w.Drain(bg) })
	})
	t.Run("durabletask", func(t *testing.T) {
		q := newQueue(t, nil)
		w := fastWorker(q, 1)
		s := serve(t, w)
		check(t, q, w, func(runs *atomic.Int32) {
			eventually(t, "the rerun", func() bool { return runs.Load() == 2 })
			time.Sleep(60 * time.Millisecond) // and no third
		})
		s.cancel()
		<-s.done
	})
}
