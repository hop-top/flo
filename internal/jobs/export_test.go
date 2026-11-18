package jobs

import (
	"context"

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
