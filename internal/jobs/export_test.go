package jobs

import (
	"context"
	"time"

	"hop.top/flo/internal/core"
)

// ClaimedBy reports the worker that holds a job's claim.
func ClaimedBy(q *Queue, id core.JobID) (string, error) {
	j, err := q.eng.Get(context.Background(), string(id))
	if err != nil {
		return "", err //nolint:wrapcheck // test helper
	}
	return j.ClaimedBy, nil
}

// ClaimAs claims the next job as worker and never settles it: a worker that
// was killed mid-job.
func ClaimAs(q *Queue, worker string) error {
	_, err := q.eng.Claim(context.Background(), QueueName, worker)
	return err //nolint:wrapcheck // test helper
}

// PruneOnce runs one prune pass of w.
func PruneOnce(w *Worker) { w.pruneOnce(context.Background()) }

// ManualHeartbeat replaces the worker's heartbeat ticker with a channel the
// test drives, and returns it.
func ManualHeartbeat(w *Worker) chan<- time.Time {
	c := make(chan time.Time)
	w.beat = func(time.Duration) (<-chan time.Time, func()) { return c, func() {} }
	return c
}
