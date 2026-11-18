package serve

import (
	"context"
	"encoding/json"

	"hop.top/flo/internal/core"
)

// Advance drives one turn of a run through the job handler the worker runs
// (which records a run started with --record), without a worker.
func (e *Engine) Advance(ctx context.Context, tenant core.TenantID, run core.RunID) error {
	b, err := json.Marshal(core.RunAdvancePayload{Tenant: tenant, Run: run})
	if err != nil {
		return err //nolint:wrapcheck // test helper
	}
	return e.advance(ctx, core.Job{Payload: b})
}
