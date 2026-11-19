package jobs_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/jobs"
)

type retentionFixture struct {
	clock                       *coretest.Clock
	q                           *jobs.Queue
	succeeded, failed, canceled core.JobID // finished two hours ago
	recent                      core.JobID // finished now
	active, pending             core.JobID
}

func (f *retentionFixture) present(t *testing.T, id core.JobID) bool {
	t.Helper()
	_, err := f.q.Get(bg, id)
	if err != nil && !core.IsCode(err, core.CodeJobNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// newRetentionFixture builds a queue holding every job state, with the clock
// moved so that three finished jobs are two hours old and one is fresh.
func newRetentionFixture(t *testing.T) *retentionFixture {
	t.Helper()
	f := &retentionFixture{clock: coretest.NewClock(time.Time{})}
	// A claim TTL far past the test so the active job stays active.
	f.q = openQueue(t, filepath.Join(t.TempDir(), "jobs.db"), f.clock, 1000*time.Hour)
	enq := func(key string, max int, at *time.Time) core.JobID {
		id, err := f.q.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: key, MaxAttempts: max, At: at})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	finish := func(ids ...core.JobID) {
		w := fastWorker(f.q, 1)
		w.Handle(core.JobRunAdvance, func(_ context.Context, j core.Job) error {
			if j.Key == "bad" {
				return errors.New("boom")
			}
			return nil
		})
		ctx, cancel := context.WithCancel(bg)
		done := make(chan struct{})
		go func() { _ = w.Serve(ctx); close(done) }()
		for _, id := range ids {
			eventually(t, "job finished", func() bool {
				i, _ := f.q.Get(bg, id)
				return i.Status == core.JobSucceeded || i.Status == core.JobFailed
			})
		}
		cancel()
		<-done
	}

	f.canceled = enq("gone", 1, nil)
	if err := f.q.Cancel(bg, f.canceled); err != nil {
		t.Fatal(err)
	}
	f.succeeded, f.failed = enq("ok", 1, nil), enq("bad", 1, nil)
	finish(f.succeeded, f.failed)
	f.clock.Advance(2 * time.Hour)

	f.recent = enq("recent", 1, nil)
	finish(f.recent)
	f.active = enq("active", 1, nil)
	if err := jobs.ClaimAs(f.q, "dead-worker"); err != nil {
		t.Fatal(err)
	}
	far := f.clock.Now().Add(100 * time.Hour)
	f.pending = enq("pending", 1, &far)
	if i, _ := f.q.Get(bg, f.active); i.Status != core.JobActive {
		t.Fatalf("active job is %v", i.Status)
	}
	return f
}

func TestPruneDeletesOnlyFinishedJobsOlderThanRetention(t *testing.T) {
	f := newRetentionFixture(t)
	n, err := f.q.Prune(bg, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("pruned %d, want 3", n)
	}
	for name, id := range map[string]core.JobID{"succeeded": f.succeeded, "failed": f.failed, "cancelled": f.canceled} {
		if f.present(t, id) {
			t.Errorf("old %s job kept", name)
		}
	}
	for name, id := range map[string]core.JobID{"recent": f.recent, "active": f.active, "pending": f.pending} {
		if !f.present(t, id) {
			t.Errorf("%s job pruned", name)
		}
	}
}

func TestPruneKeepsJobsInsideTheRetention(t *testing.T) {
	f := newRetentionFixture(t)
	if n, err := f.q.Prune(bg, 3*time.Hour); err != nil || n != 0 {
		t.Errorf("Prune(3h) = %d, %v; want 0", n, err)
	}
	f.clock.Advance(2 * time.Hour) // the fresh job is now two hours old too
	if n, err := f.q.Prune(bg, 3*time.Hour); err != nil || n != 3 {
		t.Errorf("Prune(3h) after 2h = %d, %v; want the first three", n, err)
	}
	if !f.present(t, f.recent) {
		t.Error("job inside the retention pruned")
	}
}

func TestZeroRetentionNeverPrunes(t *testing.T) {
	f := newRetentionFixture(t)
	if n, err := f.q.Prune(bg, 0); err != nil || n != 0 {
		t.Errorf("Prune(0) = %d, %v", n, err)
	}
	w := jobs.NewWorker(f.q, jobs.WorkerOptions{Pollers: 1}) // Retention zero
	jobs.PruneOnce(w)
	for name, id := range map[string]core.JobID{"succeeded": f.succeeded, "failed": f.failed, "cancelled": f.canceled, "recent": f.recent} {
		if !f.present(t, id) {
			t.Errorf("%s job pruned with retention 0", name)
		}
	}
}

func TestWorkerPrunesInTheBackground(t *testing.T) {
	f := newRetentionFixture(t)
	w := jobs.NewWorker(f.q, jobs.WorkerOptions{
		Pollers: 1, Interval: 5 * time.Millisecond, Retention: time.Hour, PruneInterval: 5 * time.Millisecond,
	})
	serve(t, w)
	eventually(t, "old jobs pruned", func() bool {
		return !f.present(t, f.succeeded) && !f.present(t, f.failed) && !f.present(t, f.canceled)
	})
	for name, id := range map[string]core.JobID{"recent": f.recent, "active": f.active, "pending": f.pending} {
		if !f.present(t, id) {
			t.Errorf("%s job pruned", name)
		}
	}
}
