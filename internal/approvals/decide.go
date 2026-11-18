package approvals

import (
	"context"
	"slices"
	"time"

	"hop.top/flo/internal/core"
)

// mutate records decisions on a copy of an approval and returns the ids of
// the items it decided. It runs again on a fresh copy when the write lost a
// race.
type mutate func(a *core.Approval, now time.Time) ([]string, error)

func allowed(set []core.Verdict, v core.Verdict) bool {
	return v != core.VerdictAuto && v.Valid() && slices.Contains(set, v)
}

// Decide implements core.ApprovalService (`flo approval set`). Items
// already decided keep their decision, so the same request twice yields the
// same state; the call that completes the approval closes its task and
// resumes the run, once.
func (s *Service) Decide(ctx context.Context, tenant core.TenantID, req core.DecideRequest) (core.DecideResult, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.DecideResult{}, err //nolint:wrapcheck // *core.Error
	}
	return s.apply(ctx, tenant, req.Approval, req.Version, func(a *core.Approval, now time.Time) ([]string, error) {
		var decided []string
		decide := func(i int, v core.ItemVerdict) error {
			if !allowed(a.Verdicts, v.Verdict) {
				return core.Errorf(core.CodeInvalidDecision, "verdict %q is not allowed here (want %v)", v.Verdict, a.Verdicts)
			}
			if a.Items[i].Decided() {
				return nil // already decided keeps its decision
			}
			a.Items[i].Decision = &core.ItemDecision{Verdict: v.Verdict, By: req.By, At: now, Reason: v.Reason, Correction: v.Correction}
			decided = append(decided, a.Items[i].ID)
			return nil
		}
		for _, iv := range req.Items {
			idx := slices.IndexFunc(a.Items, func(it core.ApprovalItem) bool { return it.ID == iv.Item })
			if idx < 0 {
				return nil, core.Errorf(core.CodeItemNotFound, "approval %s has no item %q", a.ID, iv.Item)
			}
			if err := decide(idx, iv); err != nil {
				return nil, err
			}
		}
		if req.All != "" {
			for _, i := range a.Undecided() {
				if err := decide(i, core.ItemVerdict{Verdict: req.All, Reason: req.AllReason}); err != nil {
					return nil, err
				}
			}
		}
		return decided, nil
	})
}

// apply reads the approval, lets m decide on a copy, and writes it with the
// version compare-and-set. A lost race re-reads and re-applies unless the
// caller named the version it read (then it is approval_version_conflict).
// When the write completes the approval it emits, closes the task and
// resumes the run.
func (s *Service) apply(ctx context.Context, tenant core.TenantID, id core.ApprovalID, version uint64, m mutate) (core.DecideResult, error) {
	for attempt := 0; ; attempt++ {
		a, err := s.Store.GetApproval(ctx, tenant, id)
		if err != nil {
			return core.DecideResult{}, err //nolint:wrapcheck // port error passes through
		}
		if version != 0 && a.Version != version {
			return core.DecideResult{}, core.Errorf(core.CodeApprovalConflict, "approval %s is at version %d, not %d", a.ID, a.Version, version).
				WithDetail("version", a.Version)
		}
		if a.Status == core.ApprovalCancelled {
			return core.DecideResult{}, core.Errorf(core.CodeApprovalAlreadyClosed, "approval %s was cancelled with its run", a.ID)
		}
		now := s.Clock.Now()
		next := a
		next.Items = append([]core.ApprovalItem(nil), a.Items...)
		decided, err := m(&next, now)
		if err != nil {
			return core.DecideResult{}, err
		}
		if len(decided) == 0 {
			return core.DecideResult{Approval: a}, nil
		}
		completed := next.Complete()
		if completed {
			next.Status, next.ResolvedAt = core.ApprovalDecided, &now
		}
		out, err := s.Store.UpdateApproval(ctx, next)
		if core.IsCode(err, core.CodeApprovalConflict) && version == 0 && attempt < 3 {
			continue
		}
		if err != nil {
			return core.DecideResult{}, err //nolint:wrapcheck // port error passes through
		}
		return s.settled(ctx, out, decided, completed)
	}
}

// settled runs what follows a written decision.
func (s *Service) settled(ctx context.Context, a core.Approval, decided []string, completed bool) (core.DecideResult, error) {
	for _, id := range decided {
		it, _ := a.Item(id)
		s.emit(ctx, core.TopicApprovalDecided, a, id, it.Decision.Verdict, it.Decision.By, it.Decision.Reason)
	}
	res := core.DecideResult{Approval: a, Changed: true}
	if !completed {
		return res, nil
	}
	s.emit(ctx, core.TopicApprovalResolved, a, "", "", "", "")
	s.closeTask(ctx, a)
	if err := s.Resumer.Resume(ctx, a.Tenant, a.Run); err != nil {
		return res, err //nolint:wrapcheck // port error passes through
	}
	res.Resumed = true
	return res, nil
}

// closeTask closes the approval's task as approved when any item lets the
// guarded step act, else rejected. A task that cannot be closed stays open
// for the person: the decision is recorded and does not wait on tlc.
func (s *Service) closeTask(ctx context.Context, a core.Approval) {
	if a.TaskRef == "" {
		return
	}
	outcome := core.TaskRejected
	for _, it := range a.Items {
		if it.Decision != nil && it.Decision.Verdict.Approving() {
			outcome = core.TaskApproved
		}
	}
	_ = s.Tasks.Close(ctx, a.Tenant, core.TaskRef(a.TaskRef), outcome, "decided in flo")
}
