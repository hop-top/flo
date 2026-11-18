// Package records projects flo's records onto dek's contract shapes
// (design: "flo's records take dek's shapes"): a Run is a Job, an approval
// item is a Proposal, a dry-run is a Plan. The projections are read-only
// views; flo's own records stay the source of truth. Tests validate every
// projection against the vendored dek schemas in testdata/dek.
package records

import (
	"time"

	"hop.top/flo/internal/core"
)

// Timestamp renders t as dek expects: RFC 3339 in UTC.
func Timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func stamp(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := Timestamp(*t)
	return &s
}

func text(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// JobKind is the `kind` of every run projected as a Job.
const JobKind = "flo.run"

// JobStatus is dek's Job status.
type JobStatus string

// dek job statuses flo uses.
const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobWaiting   JobStatus = "waiting"
	JobRetrying  JobStatus = "retrying"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
)

// Job is dek's Job (contract/schemas/Job.json).
type Job struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Subject     string    `json:"subject"`
	Status      JobStatus `json:"status"`
	Attempts    int       `json:"attempts"`
	MaxAttempts int       `json:"max_attempts"`
	ExpiresAt   *string   `json:"expires_at,omitempty"`
	NextRetryAt *string   `json:"next_retry_at,omitempty"`
	// Outcome is {"result": v} for a run that succeeded with a result and
	// {"failure": {code, message, ...}} for one that failed or was cancelled.
	Outcome any `json:"outcome,omitempty"`
	// WaitingFor is the event pattern flo.approval.<id> of a parked run.
	WaitingFor *string `json:"waiting_for,omitempty"`
	UpdatedAt  string  `json:"updated_at"`
}

// StatusOf maps a run status onto dek's job status: awaiting_approval is
// waiting; a queued run with a retry time is retrying.
func StatusOf(r core.Run) JobStatus {
	switch r.Status {
	case core.RunQueued:
		if r.NextRetryAt != nil {
			return JobRetrying
		}
		return JobQueued
	case core.RunRunning:
		return JobRunning
	case core.RunAwaitingApproval:
		return JobWaiting
	case core.RunSucceeded:
		return JobSucceeded
	case core.RunFailed:
		return JobFailed
	case core.RunCancelled:
		return JobCancelled
	}
	return JobFailed
}

// JobOf projects a run onto a Job. The subject is `flow/<name>`.
func JobOf(r core.Run) Job {
	j := Job{
		ID: string(r.ID), Kind: JobKind, Subject: "flow/" + r.Flow.Name, Status: StatusOf(r),
		Attempts: r.Attempts, MaxAttempts: r.MaxAttempts, ExpiresAt: stamp(r.ExpiresAt),
		NextRetryAt: stamp(r.NextRetryAt), UpdatedAt: Timestamp(r.UpdatedAt),
	}
	if j.Status == JobWaiting {
		j.WaitingFor = text(r.WaitingFor)
	}
	switch r.Status {
	case core.RunSucceeded:
		if r.Result != nil {
			j.Outcome = map[string]any{"result": r.Result}
		}
	case core.RunFailed, core.RunCancelled:
		if r.Failure != nil {
			j.Outcome = map[string]any{"failure": r.Failure}
		}
	}
	return j
}
