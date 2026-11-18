package coretest

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"hop.top/flo/internal/core"
)

// Jobs is the fake core.Jobs: an in-memory queue on the test Clock. It does
// not run anything; Worker drives it.
type Jobs struct {
	mu      sync.Mutex
	clock   core.Clock
	ids     core.IDs
	jobs    []*jobRec
	changed chan struct{} // closed and replaced on every change
}

type jobRec struct {
	info        core.JobInfo
	maxAttempts int
	seq         int
}

// NewJobs returns an empty queue.
func NewJobs(clock core.Clock, ids core.IDs) *Jobs {
	return &Jobs{clock: clock, ids: ids, changed: make(chan struct{})}
}

func (q *Jobs) notify() {
	close(q.changed)
	q.changed = make(chan struct{})
}

// Enqueue implements core.Jobs.
func (q *Jobs) Enqueue(_ context.Context, spec core.JobSpec) (core.JobID, error) {
	if err := core.RequireTenant(spec.Tenant); err != nil {
		return "", err
	}
	payload, err := json.Marshal(spec.Payload)
	if err != nil {
		return "", core.Wrap(core.CodeInternal, err)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if spec.Key != "" {
		for _, r := range q.jobs {
			if r.info.Type == spec.Type && r.info.Tenant == spec.Tenant && r.info.Key == spec.Key && !r.info.Status.Finished() {
				return r.info.ID, nil
			}
		}
	}
	now := q.clock.Now()
	at := now
	if spec.At != nil {
		at = *spec.At
	}
	max := spec.MaxAttempts
	if max < 1 {
		max = DefaultMaxAttempts
	}
	r := &jobRec{maxAttempts: max, seq: len(q.jobs) + 1, info: core.JobInfo{
		Job: core.Job{
			ID: core.JobID(q.ids.New(core.PrefixJob)), Type: spec.Type, Tenant: spec.Tenant, Key: spec.Key,
			Payload: payload, ScheduledAt: at,
		},
		Status: core.JobPending,
	}}
	q.jobs = append(q.jobs, r)
	q.notify()
	return r.info.ID, nil
}

// Cancel implements core.Jobs.
func (q *Jobs) Cancel(_ context.Context, id core.JobID) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, r := range q.jobs {
		if r.info.ID == id && !r.info.Status.Finished() {
			r.info.Status = core.JobCancelled
			q.notify()
		}
	}
	return nil
}

// Get implements core.Jobs.
func (q *Jobs) Get(_ context.Context, id core.JobID) (core.JobInfo, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, r := range q.jobs {
		if r.info.ID == id {
			return clone(r.info), nil
		}
	}
	return core.JobInfo{}, core.Errorf(core.CodeJobNotFound, "job %s not found", id)
}

// List implements core.Jobs.
func (q *Jobs) List(_ context.Context, f core.JobFilter) ([]core.JobInfo, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []core.JobInfo
	for _, r := range q.jobs {
		i := r.info
		if (f.Type != "" && i.Type != f.Type) || (f.Tenant != "" && i.Tenant != f.Tenant) || (f.Key != "" && i.Key != f.Key) {
			continue
		}
		if len(f.Statuses) > 0 && !hasJobStatus(f.Statuses, i.Status) {
			continue
		}
		out = append(out, clone(i))
	}
	return out, nil
}

func hasJobStatus(set []core.JobStatus, s core.JobStatus) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

// Pending returns the unfinished jobs, optionally of one type, ordered by
// time then enqueue order: what a test asserts the engine queued.
func (q *Jobs) Pending(t core.JobType) []core.JobInfo {
	q.mu.Lock()
	defer q.mu.Unlock()
	var recs []*jobRec
	for _, r := range q.jobs {
		if !r.info.Status.Finished() && (t == "" || r.info.Type == t) {
			recs = append(recs, r)
		}
	}
	sort.SliceStable(recs, func(i, j int) bool {
		a, b := recs[i].info.ScheduledAt, recs[j].info.ScheduledAt
		if !a.Equal(b) {
			return a.Before(b)
		}
		return recs[i].seq < recs[j].seq
	})
	out := make([]core.JobInfo, len(recs))
	for i, r := range recs {
		out[i] = clone(r.info)
	}
	return out
}

// claim takes the oldest due pending job, marking it active.
func (q *Jobs) claim() (core.Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.clock.Now()
	var best *jobRec
	for _, r := range q.jobs {
		if r.info.Status != core.JobPending || r.info.ScheduledAt.After(now) {
			continue
		}
		if best == nil || r.info.ScheduledAt.Before(best.info.ScheduledAt) ||
			(r.info.ScheduledAt.Equal(best.info.ScheduledAt) && r.seq < best.seq) {
			best = r
		}
	}
	if best == nil {
		return core.Job{}, false
	}
	best.info.Status = core.JobActive
	best.info.Attempt++
	return clone(best.info.Job), true
}

func (q *Jobs) finish(id core.JobID, handlerErr error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, r := range q.jobs {
		if r.info.ID != id {
			continue
		}
		switch {
		case r.info.Status == core.JobCancelled:
		case handlerErr == nil:
			r.info.Status = core.JobSucceeded
		case r.info.Attempt < r.maxAttempts:
			r.info.Status = core.JobPending
			r.info.Error = handlerErr.Error()
		default:
			r.info.Status = core.JobFailed
			r.info.Error = handlerErr.Error()
		}
	}
	q.notify()
}

// Worker is the fake core.JobWorker. RunDue and Drain run due jobs
// synchronously and in order, which is what a deterministic test wants;
// Serve polls for real.
type Worker struct {
	jobs     *Jobs
	mu       sync.Mutex
	handlers map[core.JobType]core.JobHandler
}

// NewWorker returns a worker over q.
func NewWorker(q *Jobs) *Worker {
	return &Worker{jobs: q, handlers: map[core.JobType]core.JobHandler{}}
}

// Handle implements core.JobWorker.
func (w *Worker) Handle(t core.JobType, h core.JobHandler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handlers[t] = h
}

// RunOne claims and runs the oldest due job. It reports false when none is
// due. A job whose type has no handler fails.
func (w *Worker) RunOne(ctx context.Context) bool {
	j, ok := w.jobs.claim()
	if !ok {
		return false
	}
	w.mu.Lock()
	h := w.handlers[j.Type]
	w.mu.Unlock()
	var err error
	if h == nil {
		err = core.Errorf(core.CodeInternal, "no handler for job type %s", j.Type)
	} else {
		err = h(ctx, j)
	}
	w.jobs.finish(j.ID, err)
	return true
}

// Drain runs due jobs, including the ones they enqueue, until none is due,
// and returns how many ran. A job that keeps requeueing itself is cut off at
// 10000 to fail a runaway test instead of hanging it.
func (w *Worker) Drain(ctx context.Context) int {
	n := 0
	for n < 10000 && ctx.Err() == nil && w.RunOne(ctx) {
		n++
	}
	return n
}

// Serve implements core.JobWorker: it runs due jobs as they appear until ctx
// is done.
func (w *Worker) Serve(ctx context.Context) error {
	for {
		w.jobs.mu.Lock()
		ch := w.jobs.changed
		w.jobs.mu.Unlock()
		for w.RunOne(ctx) {
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ch:
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Resumer is the fake core.Resumer: it enqueues the run's advance job and
// remembers the calls.
type Resumer struct {
	inner core.Resumer
	mu    sync.Mutex
	calls []core.RunID
}

// NewResumer returns a Resumer over q.
func NewResumer(q core.Jobs) *Resumer { return &Resumer{inner: core.NewJobResumer(q)} }

// Resume implements core.Resumer.
func (r *Resumer) Resume(ctx context.Context, tenant core.TenantID, run core.RunID) error {
	r.mu.Lock()
	r.calls = append(r.calls, run)
	r.mu.Unlock()
	return r.inner.Resume(ctx, tenant, run)
}

// Calls returns the runs resumed, in order.
func (r *Resumer) Calls() []core.RunID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.RunID(nil), r.calls...)
}
