package jobs

import (
	"context"
	"errors"

	"hop.top/flo/internal/core"
)

// ReconcileDeps are the ports Reconcile reads and acts through.
type ReconcileDeps struct {
	Tenants core.Tenants
	Runs    core.RunStore
	Jobs    core.Jobs
	Resumer core.Resumer
	// Approvals, when set, lets Reconcile also find a parked run whose
	// approval was settled without the resume being queued.
	Approvals core.ApprovalStore
}

// Reconcile is the startup pass: a run that should be advancing but has no
// unfinished run.advance job (the process died between storing the run and
// queueing its job, or after the job was lost) gets one. It returns the runs
// it re-queued. A run waiting in its group's queue is not an orphan: its job
// is made when a slot frees.
func Reconcile(ctx context.Context, d ReconcileDeps) ([]core.RunID, error) {
	tenants, err := d.Tenants.List(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // port error passes through
	}
	var fixed []core.RunID
	var errs []error
	for _, t := range tenants {
		ids, err := reconcileTenant(ctx, d, t)
		fixed = append(fixed, ids...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return fixed, errors.Join(errs...)
}

func reconcileTenant(ctx context.Context, d ReconcileDeps, t core.TenantID) ([]core.RunID, error) {
	pending, err := d.Jobs.List(ctx, core.JobFilter{
		Type: core.JobRunAdvance, Tenant: t, Statuses: []core.JobStatus{core.JobPending, core.JobActive},
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // port error passes through
	}
	has := map[string]bool{}
	for _, j := range pending {
		has[j.Key] = true
	}
	runs, err := d.Runs.ListRuns(ctx, t, core.RunFilter{
		Statuses: []core.RunStatus{core.RunQueued, core.RunRunning, core.RunAwaitingApproval}, Limit: 1 << 20,
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // port error passes through
	}
	var fixed []core.RunID
	var errs []error
	for _, r := range runs {
		if has[string(r.ID)] || !needsAdvance(ctx, d, r) {
			continue
		}
		if err := d.Resumer.Resume(ctx, t, r.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		fixed = append(fixed, r.ID)
	}
	return fixed, errors.Join(errs...)
}

func needsAdvance(ctx context.Context, d ReconcileDeps, r core.Run) bool {
	switch r.Status {
	case core.RunQueued:
		return r.Admitted
	case core.RunRunning:
		return true
	case core.RunAwaitingApproval:
		return settledApproval(ctx, d, r)
	}
	return false
}

// settledApproval reports a parked run whose approval is no longer open.
func settledApproval(ctx context.Context, d ReconcileDeps, r core.Run) bool {
	if d.Approvals == nil {
		return false
	}
	aps, err := d.Approvals.ListApprovals(ctx, r.Tenant, core.ApprovalFilter{Run: r.ID})
	if err != nil {
		return false
	}
	for _, a := range aps {
		if core.WaitingForApproval(a.ID) == r.WaitingFor {
			return a.Status != core.ApprovalOpen
		}
	}
	return false
}
