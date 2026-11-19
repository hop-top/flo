// Package jobs is the delayed-job queue the engine runs on: kit's durabletask
// engine behind core.Jobs, a worker that heartbeats claims, the Resumer and
// the startup reconciler. One jobs database serves every tenant (D3); the
// tenant travels in the job's metadata and payload.
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"hop.top/kit/go/runtime/domain"
	"hop.top/kit/go/runtime/job"
	"hop.top/kit/go/runtime/job/durabletask"
	"hop.top/kit/go/storage/sqldb"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/store/sqlite"
)

const (
	// QueueName is the one kit queue flo uses.
	QueueName = "flo"
	// DefaultMaxAttempts bounds job pickups when the spec names none.
	DefaultMaxAttempts = 5
	// DefaultClaimTTL is how long a claim lives without a heartbeat.
	DefaultClaimTTL = 5 * time.Minute

	reservationStale = 30 * time.Second
	reserveRetry     = 5 * time.Millisecond
)

// Options configure Open.
type Options struct {
	// Clock drives job scheduling and claim expiry; the system clock when nil.
	Clock core.Clock
	// ClaimTTL is how long a claim lives without a heartbeat; the worker
	// heartbeats at a third of it. Zero takes DefaultClaimTTL.
	ClaimTTL time.Duration
	// Backoff is the retry backoff of a failed attempt; zero takes kit's.
	Backoff job.BackoffStrategy
	// NoSync turns fsync off, for throwaway test databases only.
	NoSync bool
}

// Queue implements core.Jobs.
type Queue struct {
	eng  *durabletask.Engine
	db   *sql.DB // the same file: the key table that dedupes enqueues
	opts Options
}

var _ core.Jobs = (*Queue)(nil)

const keySchema = `
CREATE TABLE IF NOT EXISTS flo_job_keys (
	type   TEXT NOT NULL,
	tenant TEXT NOT NULL,
	key    TEXT NOT NULL,
	job_id TEXT NOT NULL DEFAULT '',
	at     INTEGER NOT NULL,
	rerun  INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (type, tenant, key)
);
CREATE INDEX IF NOT EXISTS flo_job_keys_job ON flo_job_keys (job_id);
`

// Open opens (creating) the jobs database at path.
func Open(path string, opts Options) (*Queue, error) {
	if opts.Clock == nil {
		opts.Clock = core.SystemClock{}
	}
	if opts.ClaimTTL <= 0 {
		opts.ClaimTTL = DefaultClaimTTL
	}
	eng, err := durabletask.New(path, job.WithNowFunc(opts.Clock.Now))
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err)
	}
	db, err := sqlite.OpenDB(path, 0, opts.NoSync)
	if err != nil {
		_ = eng.Close()
		return nil, core.Wrap(core.CodeInternal, err)
	}
	q := &Queue{eng: eng, db: db, opts: opts}
	if err := q.migrate(); err != nil {
		_ = q.Close()
		return nil, core.Wrap(core.CodeInternal, err)
	}
	return q, nil
}

func (q *Queue) migrate() error {
	err := sqldb.Migrate(q.db, "flo_jobs_migrations", map[int]string{1: keySchema})
	if err != nil {
		err = sqldb.Migrate(q.db, "flo_jobs_migrations", map[int]string{1: keySchema})
	}
	if err != nil {
		return fmt.Errorf("jobs migrate: %w", err)
	}
	return nil
}

// Close releases both connections.
func (q *Queue) Close() error {
	return errors.Join(q.db.Close(), q.eng.Close())
}

// ClaimTTL is the claim lifetime jobs are enqueued with.
func (q *Queue) ClaimTTL() time.Duration { return q.opts.ClaimTTL }

// ReleaseStaleClaims returns jobs whose claim expired (a killed worker) to
// pending and reports how many.
func (q *Queue) ReleaseStaleClaims(ctx context.Context) (int, error) {
	n, err := q.eng.ReleaseStaleClaims(ctx)
	return n, internal(err)
}

func internal(err error) error {
	if err == nil {
		return nil
	}
	return core.Wrap(core.CodeInternal, err)
}

func unfinished(s job.Status) bool {
	return s == job.StatusPending || s == job.StatusActive
}

// Enqueue implements core.Jobs.
func (q *Queue) Enqueue(ctx context.Context, spec core.JobSpec) (core.JobID, error) {
	if err := core.RequireTenant(spec.Tenant); err != nil {
		return "", err
	}
	if spec.Type == "" {
		return "", core.Errorf(core.CodeUsage, "a job needs a type")
	}
	if spec.Key == "" {
		return q.insert(ctx, spec)
	}
	for {
		owner, existing, err := q.reserve(ctx, spec)
		if err != nil {
			return "", err
		}
		if existing != "" {
			return existing, nil
		}
		if owner {
			break
		}
		select { // another enqueuer holds the key mid-flight
		case <-ctx.Done():
			return "", ctx.Err() //nolint:wrapcheck // the caller's own cancellation
		case <-time.After(reserveRetry):
		}
	}
	id, err := q.insert(ctx, spec)
	if err != nil {
		_, _ = q.db.ExecContext(context.WithoutCancel(ctx), `DELETE FROM flo_job_keys WHERE type = ? AND tenant = ? AND key = ? AND job_id = ''`,
			string(spec.Type), string(spec.Tenant), spec.Key)
		return "", err
	}
	if _, err := q.db.ExecContext(ctx, `UPDATE flo_job_keys SET job_id = ?, at = ? WHERE type = ? AND tenant = ? AND key = ?`,
		string(id), time.Now().UnixNano(), string(spec.Type), string(spec.Tenant), spec.Key); err != nil {
		return id, internal(err)
	}
	return id, nil
}

// reserve claims the right to enqueue (type, tenant, key), or finds the job
// that already holds it. It runs in one write transaction, so two processes
// cannot both become owner. The reservation is empty until insert fills in
// the job id; one left behind by a crash is taken over after a short while.
func (q *Queue) reserve(ctx context.Context, spec core.JobSpec) (owner bool, existing core.JobID, err error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", internal(err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	typ, ten := string(spec.Type), string(spec.Tenant)
	var id string
	var at int64
	now := time.Now().UnixNano()
	switch err = tx.QueryRowContext(ctx, `SELECT job_id, at FROM flo_job_keys WHERE type = ? AND tenant = ? AND key = ?`, typ, ten, spec.Key).Scan(&id, &at); {
	case errors.Is(err, sql.ErrNoRows):
		if _, err = tx.ExecContext(ctx, `INSERT INTO flo_job_keys (type, tenant, key, job_id, at) VALUES (?, ?, ?, '', ?)`, typ, ten, spec.Key, now); err != nil {
			return false, "", internal(err)
		}
		return true, "", internal(tx.Commit())
	case err != nil:
		return false, "", internal(err)
	}
	switch {
	case id != "":
		j, gerr := q.eng.Get(ctx, id)
		if gerr == nil && j.Status == job.StatusActive {
			// Running now: what the caller wants done may have been missed by
			// this turn, so ask for another once it settles (see afterSettle).
			// Re-read after flagging: a job that settled in between has already
			// looked at the flag, so this enqueue must make the new job itself.
			if _, err = tx.ExecContext(ctx, `UPDATE flo_job_keys SET rerun = 1 WHERE type = ? AND tenant = ? AND key = ?`, typ, ten, spec.Key); err != nil {
				return false, "", internal(err)
			}
			j, gerr = q.eng.Get(ctx, id)
		}
		if gerr == nil && unfinished(j.Status) {
			return false, core.JobID(id), internal(tx.Commit())
		}
		if gerr != nil && !errors.Is(gerr, domain.ErrNotFound) {
			err = gerr
			return false, "", internal(err)
		}
	case time.Duration(now-at) < reservationStale:
		return false, "", internal(tx.Commit()) // someone else is enqueuing it
	}
	if _, err = tx.ExecContext(ctx, `UPDATE flo_job_keys SET job_id = '', at = ?, rerun = 0 WHERE type = ? AND tenant = ? AND key = ?`, now, typ, ten, spec.Key); err != nil {
		return false, "", internal(err)
	}
	return true, "", internal(tx.Commit())
}

func (q *Queue) insert(ctx context.Context, spec core.JobSpec) (core.JobID, error) {
	max := spec.MaxAttempts
	if max < 1 {
		max = DefaultMaxAttempts
	}
	o := job.EnqueueOpts{
		Queue: QueueName, Type: string(spec.Type), MaxAttempts: max, ClaimTTL: q.opts.ClaimTTL, Backoff: q.opts.Backoff,
		ScheduledAt: spec.At, Metadata: map[string]any{"tenant": string(spec.Tenant), "key": spec.Key},
	}
	if spec.Payload != nil {
		o.Payload = spec.Payload
	}
	id, err := q.eng.Enqueue(ctx, o) // kit ids are full-length typeids: no collision to retry
	if err != nil {
		return "", internal(err)
	}
	return core.JobID(id), nil
}

// Prune deletes finished jobs (succeeded, failed, timed out, cancelled) last
// updated more than retention ago, by the queue's clock, and returns how many.
// Pending and active jobs are never touched. A retention of zero or less
// deletes nothing.
func (q *Queue) Prune(ctx context.Context, retention time.Duration) (int, error) {
	if retention <= 0 {
		return 0, nil
	}
	n, err := q.eng.Prune(ctx, q.opts.Clock.Now().Add(-retention))
	if err != nil {
		return 0, internal(err)
	}
	return n, nil
}

func toInfo(j *job.Job) core.JobInfo {
	ten, _ := j.Metadata["tenant"].(string)
	key, _ := j.Metadata["key"].(string)
	at := j.CreatedAt
	if j.ScheduledAt != nil {
		at = *j.ScheduledAt
	}
	var payload []byte
	if j.Payload != nil {
		payload = []byte(j.Payload)
	}
	return core.JobInfo{
		Job: core.Job{
			ID: core.JobID(j.ID), Type: core.JobType(j.Type), Tenant: core.TenantID(ten), Key: key,
			Payload: payload, Attempt: j.Attempts, ScheduledAt: at,
		},
		Status: statusOf(j.Status), Error: j.Error,
	}
}

func statusOf(s job.Status) core.JobStatus {
	switch s {
	case job.StatusPending:
		return core.JobPending
	case job.StatusActive:
		return core.JobActive
	case job.StatusSucceeded:
		return core.JobSucceeded
	case job.StatusCancelled:
		return core.JobCancelled
	}
	return core.JobFailed // failed and timeout
}

func kitStatuses(s core.JobStatus) []string {
	switch s {
	case core.JobPending:
		return []string{string(job.StatusPending)}
	case core.JobActive:
		return []string{string(job.StatusActive)}
	case core.JobSucceeded:
		return []string{string(job.StatusSucceeded)}
	case core.JobCancelled:
		return []string{string(job.StatusCancelled)}
	case core.JobFailed:
		return []string{string(job.StatusFailed), string(job.StatusTimeout)}
	}
	return nil
}

// Get implements core.Jobs.
func (q *Queue) Get(ctx context.Context, id core.JobID) (core.JobInfo, error) {
	j, err := q.eng.Get(ctx, string(id))
	if errors.Is(err, domain.ErrNotFound) {
		return core.JobInfo{}, core.Errorf(core.CodeJobNotFound, "job %s not found", id)
	}
	if err != nil {
		return core.JobInfo{}, internal(err)
	}
	return toInfo(j), nil
}

// Cancel implements core.Jobs.
func (q *Queue) Cancel(ctx context.Context, id core.JobID) error {
	j, err := q.eng.Get(ctx, string(id))
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return internal(err)
	}
	if !unfinished(j.Status) {
		return nil
	}
	if err := q.eng.Cancel(ctx, string(id)); err != nil {
		// lost a race with the job finishing
		if j2, gerr := q.eng.Get(ctx, string(id)); gerr == nil && !unfinished(j2.Status) {
			return nil
		}
		return internal(err)
	}
	return nil
}

// List implements core.Jobs.
func (q *Queue) List(ctx context.Context, f core.JobFilter) ([]core.JobInfo, error) {
	var kits []job.Job
	statuses := []string{""}
	if len(f.Statuses) > 0 {
		statuses = nil
		for _, s := range f.Statuses {
			statuses = append(statuses, kitStatuses(s)...)
		}
	}
	for _, st := range statuses {
		js, err := q.eng.List(ctx, job.JobQuery{Queue: QueueName, Type: string(f.Type), Status: st})
		if err != nil {
			return nil, internal(err)
		}
		kits = append(kits, js...)
	}
	sort.SliceStable(kits, func(i, j int) bool { return kits[i].CreatedAt.Before(kits[j].CreatedAt) })
	var out []core.JobInfo
	for i := range kits {
		info := toInfo(&kits[i])
		if (f.Tenant != "" && info.Tenant != f.Tenant) || (f.Key != "" && info.Key != f.Key) {
			continue
		}
		out = append(out, info)
	}
	return out, nil
}

// afterSettle runs once a claimed job has been completed or failed for good:
// if an Enqueue of the same (type, tenant, key) arrived while it ran, the key
// is queued again, so a resume that landed mid-turn is not lost. A job that
// goes back to the queue for a retry needs no extra one.
func (q *Queue) afterSettle(ctx context.Context, j *job.Job) {
	ten, _ := j.Metadata["tenant"].(string)
	key, _ := j.Metadata["key"].(string)
	if key == "" {
		return
	}
	var rerun int
	err := q.db.QueryRowContext(ctx, `SELECT rerun FROM flo_job_keys WHERE type = ? AND tenant = ? AND key = ? AND job_id = ?`,
		j.Type, ten, key, j.ID).Scan(&rerun)
	if err != nil || rerun == 0 {
		return
	}
	cur, err := q.eng.Get(ctx, j.ID)
	if err != nil || unfinished(cur.Status) {
		_, _ = q.db.ExecContext(ctx, `UPDATE flo_job_keys SET rerun = 0 WHERE type = ? AND tenant = ? AND key = ? AND job_id = ?`, j.Type, ten, key, j.ID)
		return
	}
	spec := core.JobSpec{Type: core.JobType(j.Type), Tenant: core.TenantID(ten), Key: key, MaxAttempts: j.MaxAttempts}
	if j.Payload != nil {
		spec.Payload = j.Payload
	}
	_, _ = q.Enqueue(ctx, spec) // reserve sees the finished job and takes the key over, clearing the flag
}
