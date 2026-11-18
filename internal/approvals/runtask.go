package approvals

import (
	"bytes"
	"context"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec"
)

// Task modes of a flow (flow.md §7.8).
const (
	tasksNone    = "none"
	tasksPerRun  = "per-run"
	tasksPerStep = "per-step"
)

// runTaskMode reads the `tasks` key of the run's flow version. A flow that
// cannot be read or parsed, and one that does not say, is per-run.
func (s *Service) runTaskMode(ctx context.Context, r core.Run) string {
	if s.Flows == nil {
		return tasksPerRun
	}
	v, err := s.Flows.GetVersion(ctx, r.Tenant, r.Flow.Name, r.Flow.Hash)
	if err != nil {
		return tasksPerRun
	}
	f, err := spec.ParseFlow(bytes.NewReader(v.Source), r.Flow.Name)
	if err != nil || f.Tasks == "" {
		return tasksPerRun
	}
	return f.Tasks
}

// ensureRunTask returns the accountable task of the run, creating it the
// first time the run needs a person (flow.md §7.8): titled
// `<flow> <trigger instant>`, due at the trigger instant. A flow with
// `tasks: none` or `per-step` has none; its approval tasks stand alone.
func (s *Service) ensureRunTask(ctx context.Context, tenant core.TenantID, run core.RunID) (core.TaskRef, error) {
	if s.Runs == nil {
		return "", nil
	}
	r, err := s.Runs.GetRun(ctx, tenant, run)
	if err != nil {
		return "", err //nolint:wrapcheck // port error passes through
	}
	if r.TaskRef != "" {
		return core.TaskRef(r.TaskRef), nil
	}
	if mode := s.runTaskMode(ctx, r); mode == tasksNone || mode == tasksPerStep {
		return "", nil
	}
	at := r.Trigger.At
	if at.IsZero() {
		at = r.CreatedAt
	}
	ref, err := s.Tasks.Create(ctx, tenant, core.TaskSpec{
		Title: r.Flow.Name + " " + at.UTC().Format(time.RFC3339), Due: &at, Run: run, Tags: []string{"run-task"},
	})
	if err != nil {
		return "", err //nolint:wrapcheck // port error passes through
	}
	refText := string(ref)
	if _, err := s.Runs.UpdateRun(ctx, tenant, run, nil, core.RunPatch{TaskRef: &refText}); err != nil {
		return "", err //nolint:wrapcheck // port error passes through
	}
	return ref, nil
}
