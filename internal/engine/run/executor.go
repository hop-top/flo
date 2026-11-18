// Package run is the run executor: the run.advance job handler. One job
// advances one run (D2); inside it the ready steps run concurrently behind
// the Runner seam until every step is terminal, the run parks on an approval,
// or it is cancelled or over budget.
//
// A turn is stateless across restarts: it re-reads the run, the flow version
// and the step records, so a run resumed after a park or a crash picks up
// where it was and never re-runs a step that succeeded.
//
// Layout:
//
//	executor.go  Executor, Config, the job handler, turn start and finish
//	plan.go      flow version -> expanded, compiled steps
//	turn.go      the scheduler loop: readiness, launch, park, cancel
//	unit.go      one step or one for_each element: render, access, attempts, check
//	foreach.go   for_each fan-out and aggregation
//	access.go    grant, irreversible guard, admission
//	events.go    run-log emission
package run

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"hop.top/flo/internal/core"
)

// Config tunes an Executor. The zero value is usable.
type Config struct {
	// MaxParallel bounds the attempts in flight across one run's steps and
	// elements (flow.md §5.4, the engine's worker limit). Default 16.
	MaxParallel int
	// CancelPoll is how often a running turn looks for a cancel request.
	// Default 500ms; negative disables polling (the flag is still read
	// between steps).
	CancelPoll time.Duration
	// Config is the read-only `config` namespace of expressions, and the
	// source of `config.eva.url`.
	Config map[string]any
}

const (
	defaultMaxParallel = 16
	defaultCancelPoll  = 500 * time.Millisecond
)

// Executor advances runs. Create it with New and register it on a worker.
type Executor struct {
	d   *core.Services
	cfg Config
}

// New returns an Executor over the engine's services.
func New(svc *core.Services, cfg Config) *Executor {
	if cfg.MaxParallel < 1 {
		cfg.MaxParallel = defaultMaxParallel
	}
	if cfg.CancelPoll == 0 {
		cfg.CancelPoll = defaultCancelPoll
	}
	return &Executor{d: svc, cfg: cfg}
}

// Register installs the run.advance handler on w.
func (e *Executor) Register(w core.JobWorker) { w.Handle(core.JobRunAdvance, e.Handle) }

// Handle is the run.advance job handler. It returns an error only for a
// fault of the engine itself (a store that failed): a failed or parked run
// is recorded data and finishes the job.
func (e *Executor) Handle(ctx context.Context, j core.Job) error {
	var p core.RunAdvancePayload
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return core.Errorf(core.CodeInternal, "run.advance payload: %w", err)
	}
	return e.Advance(ctx, p.Tenant, p.Run)
}

// Advance runs one turn of a run: from where it stands to its next park or
// its terminal status.
func (e *Executor) Advance(ctx context.Context, tenant core.TenantID, id core.RunID) error {
	run, err := e.d.Runs.GetRun(ctx, tenant, id)
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	if run.Status.Terminal() {
		return nil
	}
	p, perr := e.loadPlan(ctx, run)
	t := e.newTurn(ctx, run, p)
	defer t.close()
	switch {
	case run.CancelRequested:
		return t.cancelRun(ctx)
	case !run.Admitted && run.Status == core.RunQueued:
		return nil // a stale job: the run still waits for a slot
	case perr != nil:
		return t.finish(core.RunFailed, core.FailureOf(perr), nil)
	}
	if err := t.begin(ctx); err != nil {
		return err
	}
	return t.loop(ctx)
}

// stateSnapshot is the flow state at run start: what the store holds, with
// the declared default for every key it does not.
func (e *Executor) stateSnapshot(ctx context.Context, run core.Run, p *plan) (map[string]any, error) {
	stored, err := e.d.State.Snapshot(ctx, run.Tenant, run.Flow.Name)
	if err != nil {
		return nil, err //nolint:wrapcheck // port error passes through
	}
	snap := map[string]any{}
	for k, decl := range p.settings.State {
		if v, ok := stored[k]; ok {
			snap[k] = v
		} else {
			snap[k] = decl.Default
		}
	}
	for k, v := range stored {
		if _, ok := snap[k]; !ok {
			snap[k] = v
		}
	}
	out, err := core.NormalizeJSON(snap)
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err)
	}
	m, _ := out.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func describe(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprint(err)
}
