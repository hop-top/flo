package serve

import (
	"context"
	"os"
	"time"

	"hop.top/flo/internal/jobs"
	"hop.top/flo/internal/version"
)

// DefaultShutdownGrace is how long Serve lets running steps finish after its
// context is done before cancelling them; a cancelled turn is picked up again
// by the next engine (the step it interrupted is recovered, not repeated if it
// had succeeded).
const DefaultShutdownGrace = 30 * time.Second

// Serve runs the engine until ctx is done: the start-up pass, then the worker
// pollers that advance runs, fire triggers and poll approvals. When ctx is
// done it stops claiming and waits for running steps to finish (drain), up to
// the shutdown grace.
//
// The start-up pass queues what a crash left unqueued: a run advance job lost
// between storing the run and queueing it, a schedule's next fire, a calendar trigger's sync, the next
// approval poll; and it releases claims of workers that stopped heartbeating.
func (e *Engine) Serve(ctx context.Context) error {
	host, _ := os.Hostname()
	release, err := claimState(e.Config.Root, State{
		PID: os.Getpid(), Host: host, Since: e.Config.Clock.Now().UTC(), Version: version.Version(),
		Tenant: string(e.Config.Tenant), Pollers: e.pollers(), Root: e.Config.Root,
	})
	if err != nil {
		return err
	}
	defer release()
	if err := e.startup(ctx); err != nil {
		return err
	}
	e.Config.Logger.Info("engine serving", "root", e.Config.Root, "tenant", e.Config.Tenant, "pollers", e.pollers())
	if err := e.Worker.Serve(ctx); err != nil {
		return err //nolint:wrapcheck // the worker's own error
	}
	e.Config.Logger.Info("engine stopped")
	return nil
}

func (e *Engine) pollers() int {
	if e.Config.Pollers > 0 {
		return e.Config.Pollers
	}
	return 4 // jobs.NewWorker's default
}

func (e *Engine) startup(ctx context.Context) error {
	if _, err := e.Queue.ReleaseStaleClaims(ctx); err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	if _, err := jobs.Reconcile(ctx, jobs.ReconcileDeps{
		Tenants: e.Services.Tenants, Runs: e.Services.Runs, Jobs: e.Queue, Resumer: e.Services.Resumer, Approvals: e.Services.Approvals,
	}); err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	if err := e.Scheduler.Reconcile(ctx); err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	if err := e.Calendar.Reconcile(ctx); err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	ts, err := e.Services.Tenants.List(ctx)
	if err != nil {
		return err //nolint:wrapcheck // *core.Error
	}
	for _, t := range ts {
		if err := e.Approvals.EnsurePoll(ctx, t); err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
		if err := e.Pruner.Ensure(ctx, t); err != nil {
			return err //nolint:wrapcheck // *core.Error
		}
	}
	return nil
}
