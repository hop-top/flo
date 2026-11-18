package core

import (
	"context"
	"time"

	"hop.top/kit/go/runtime/bus"

	specrun "hop.top/flo/spec/run"
)

// Clock is the engine's time. It is a port so tests drive time: the
// executor's backoffs, parked-time accounting and schedule maths all read it.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// Sleep blocks until d has elapsed on this clock or ctx is done, and
	// returns ctx.Err() in that case. d <= 0 returns at once.
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock is the wall clock.
type SystemClock struct{}

// Now implements Clock.
func (SystemClock) Now() time.Time { return time.Now() }

// Sleep implements Clock.
func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // the caller's own cancellation
	}
}

// Jobs is the delayed-job queue the engine runs on (kit job, durabletask).
// It knows jobs, not runs.
type Jobs interface {
	// Enqueue queues a job. At most one unfinished job exists per
	// (Type, Tenant, Key): when one does, its id is returned and nothing is
	// queued, so a duplicate of a pending job changes nothing. When that job
	// is active (running now) the request may have been missed by its turn:
	// the id is returned and one more job with the same payload is queued
	// when it settles; any number of such requests make one rerun. A job
	// that goes back to the queue for a retry needs no rerun. An empty Key
	// never dedupes.
	Enqueue(ctx context.Context, spec JobSpec) (JobID, error)
	// Cancel cancels a job that has not finished. Cancelling a finished or
	// unknown job is not an error.
	Cancel(ctx context.Context, id JobID) error
	// Get returns a job, or job_not_found.
	Get(ctx context.Context, id JobID) (JobInfo, error)
	// List returns jobs matching the filter, oldest first.
	List(ctx context.Context, f JobFilter) ([]JobInfo, error)
}

// JobHandler handles one claimed job. Returning nil finishes it, including
// when the work ended in a recorded domain failure (a failed run is data,
// not a job failure). Returning an error makes the queue retry or fail it.
type JobHandler func(ctx context.Context, j Job) error

// JobWorker claims jobs and hands them to the handler of their type. It
// heartbeats claims while a handler runs.
type JobWorker interface {
	// Handle registers the handler of a job type; one handler per type.
	Handle(t JobType, h JobHandler)
	// Serve polls until ctx is done, then waits for running handlers to
	// finish (drain).
	Serve(ctx context.Context) error
}

// Resumer makes a parked or queued run advance: it enqueues a run.advance
// job for it. It is idempotent: a run with a pending advance job gets no
// second one.
type Resumer interface {
	Resume(ctx context.Context, tenant TenantID, run RunID) error
}

// GroupAction is what a new run does against its concurrency group.
type GroupAction string

// Group actions.
const (
	// GroupRun: a slot was reserved; the run starts.
	GroupRun GroupAction = "run"
	// GroupQueue: the group is full; the run waits for a slot (FIFO).
	GroupQueue GroupAction = "queue"
	// GroupSkip: the group is full and on_overlap is skip; no run is made.
	GroupSkip GroupAction = "skip"
)

// GroupRequest asks for a slot in a concurrency group (flow.md §7.3).
type GroupRequest struct {
	Tenant    TenantID
	Group     string
	Max       int
	OnOverlap specrun.OnOverlap
	// Run is the id of the run that will hold the slot; the caller creates
	// the run with this id and Admitted set per the decision.
	Run RunID
}

// GroupDecision is the answer to a GroupRequest.
type GroupDecision struct {
	Action GroupAction
	// Cancel lists the active runs of the group the caller must cancel
	// (on_overlap cancel_previous); the new run starts at once.
	Cancel []RunID
}

// Groups arbitrates concurrency groups. A run holds a slot from admission to
// its terminal status; a parked run holds its slot (flow.md §7.3). The
// occupancy is derived from the admitted, unfinished runs plus open
// reservations, not from a second counter.
type Groups interface {
	// Admit decides what a new run does. For GroupRun it reserves the slot
	// atomically, so two simultaneous fires cannot both take the last one;
	// the reservation lasts until CreateRun with the same run id, Abandon, or
	// one minute. Asking again for a run that holds a reservation or a slot
	// returns the same decision.
	Admit(ctx context.Context, req GroupRequest) (GroupDecision, error)
	// Abandon drops the reservation of a run that was never created (the
	// create found the run key already used). Unknown runs are not an error.
	Abandon(ctx context.Context, tenant TenantID, group string, run RunID) error
	// Release promotes queued runs of the group into free slots, oldest
	// first, marking them admitted, and returns them; the caller enqueues
	// their run.advance jobs. It is called after a run of the group finishes.
	Release(ctx context.Context, tenant TenantID, group string) ([]RunID, error)
}

// EventReader reads a run's log.
type EventReader interface {
	// Next returns the next event in order. It returns io.EOF after the last
	// event when the reader does not follow, and after the run's terminal
	// event when it does; a following reader blocks for new events until ctx
	// is done.
	Next(ctx context.Context) (bus.Event, error)
	// Close releases the reader.
	Close() error
}

// RunLog is the run's ordered log of CloudEvents (flo.run.*). It writes to
// kit's bus and to the run's JSON Lines file.
type RunLog interface {
	// Emit appends an event to a run's log (run "" for engine events) and
	// publishes it. The log stamps the ExtSeq extension with the run's
	// next sequence number. Emitting never fails the caller's work: an error
	// is reported but a handler may ignore it.
	Emit(ctx context.Context, tenant TenantID, run RunID, ev bus.Event) error
	// Open reads a run's events from the first. With follow it keeps
	// delivering as events are appended.
	Open(ctx context.Context, tenant TenantID, run RunID, follow bool) (EventReader, error)
}

// StartRequest creates a run of a flow: a manual `flo run create`, a trigger
// fire, a retry from the brief.
type StartRequest struct {
	Tenant TenantID
	Flow   string
	// Version pins a flow version (hash or document version). Empty starts
	// the active version.
	Version string
	// Vars are the bound, typed vars.
	Vars    map[string]any
	Trigger TriggerInfo
	// IdemKey is the run key: a second start with it returns the first run.
	IdemKey string
	// StartedBy is the calling profile.
	StartedBy string
	// Record asks the engine to record the run's exec and HTTP traffic for
	// offline replay (`flo run replay`).
	Record bool
}

// StartResult is what Start did.
type StartResult struct {
	Run Run
	// Created is false when IdemKey already had a run: Run is that run.
	Created bool
	// Skipped is true when the concurrency group was full and on_overlap is
	// skip: no run was made and Run is the zero value.
	Skipped bool
	// Queued is true when the run waits for a slot.
	Queued bool
	// Cancelled lists the runs cancelled by on_overlap cancel_previous.
	Cancelled []RunID
}

// Starter creates runs. It is the one path from a request to a queued run,
// shared by triggers and the CLI: resolve the flow version, settle the run
// key, take a concurrency slot, store the run, enqueue its advance job.
// NewStarter is the implementation.
type Starter interface {
	// Start creates a run per the request.
	Start(ctx context.Context, req StartRequest) (StartResult, error)
	// Release frees what a finished run held: it promotes queued runs of its
	// group and enqueues them. The executor calls it after FinishRun.
	Release(ctx context.Context, run Run) error
}

// TriggerSync registers a flow version's triggers and queues their first
// fires; it is called when a version is activated.
type TriggerSync interface {
	// Sync makes the registrations of the flow match the version: new
	// triggers are registered and their first fire queued, triggers the
	// version no longer declares are removed and their jobs cancelled.
	Sync(ctx context.Context, tenant TenantID, v FlowVersion) error
	// Unsync cancels a flow's queued fires and pauses its registrations
	// (the flow was paused).
	Unsync(ctx context.Context, tenant TenantID, flow string) error
}
