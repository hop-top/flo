package brief

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"hop.top/flo/internal/core"
)

// JobPrune is the daily job that deletes outputs older than the retention.
// Payload: PrunePayload. Key: the period it runs in.
const JobPrune core.JobType = "outputs.prune"

// PruneEvery is the period of the prune job.
const PruneEvery = 24 * time.Hour

// PrunePayload is the payload of a JobPrune job.
type PrunePayload struct {
	Tenant core.TenantID `json:"tenant"`
}

// Pruner keeps the outputs of every tenant to core.OutputRetention.
type Pruner struct {
	Outputs core.OutputStore
	Jobs    core.Jobs
	Clock   core.Clock
}

// Register hands the outputs.prune handler to the job worker.
func (p *Pruner) Register(w core.JobWorker) { w.Handle(JobPrune, p.Handle) }

// Ensure queues the prune that follows the current day, once: the key carries
// the period, so every caller in a day shares one job.
func (p *Pruner) Ensure(ctx context.Context, tenant core.TenantID) error {
	next := p.Clock.Now().Truncate(PruneEvery).Add(PruneEvery)
	_, err := p.Jobs.Enqueue(ctx, core.JobSpec{
		Type: JobPrune, Tenant: tenant, Key: "prune@" + strconv.FormatInt(next.Unix(), 10),
		Payload: PrunePayload{Tenant: tenant}, At: &next,
	})
	return err //nolint:wrapcheck // port error passes through
}

// Handle is the core.JobHandler of JobPrune. It queues the next day's prune
// first, so a failing prune is retried by the queue and does not stop the
// schedule.
func (p *Pruner) Handle(ctx context.Context, j core.Job) error {
	var pl PrunePayload
	if err := json.Unmarshal(j.Payload, &pl); err != nil {
		return core.Errorf(core.CodeInternal, "outputs.prune payload: %w", err)
	}
	if pl.Tenant == "" {
		pl.Tenant = j.Tenant
	}
	if err := p.Ensure(ctx, pl.Tenant); err != nil {
		return err
	}
	_, err := p.Prune(ctx, pl.Tenant)
	return err
}

// Prune deletes the tenant's outputs older than the retention and reports how
// many.
func (p *Pruner) Prune(ctx context.Context, tenant core.TenantID) (int, error) {
	return p.Outputs.PruneOutputs(ctx, tenant, p.Clock.Now().Add(-core.OutputRetention)) //nolint:wrapcheck // port error passes through
}
