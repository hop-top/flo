package jobs

import "hop.top/flo/internal/core"

// NewResumer returns the Resumer over q: it enqueues the run's run.advance
// job, one per run (the key dedupes a second request while one is pending).
func NewResumer(q core.Jobs) core.Resumer { return core.NewJobResumer(q) }
