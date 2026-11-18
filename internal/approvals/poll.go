package approvals

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"hop.top/flo/internal/core"
)

// DeciderTLC is who decided an item when the person closed its task in tlc
// and tlc did not say who.
const DeciderTLC = "tlc"

// Reasons of decisions the poll makes.
const (
	ReasonTaskApproved = "task approved"
	ReasonTaskRejected = "task rejected"
)

// PollResult is what one poll did.
type PollResult struct {
	// Open is how many approvals were open before the poll.
	Open int
	// FromTasks counts approvals completed by a closed task, TimedOut those
	// completed by a timeout.
	FromTasks int
	TimedOut  int
}

// Register hands the approval.poll handler to the job worker.
func (s *Service) Register(w core.JobWorker) { w.Handle(core.JobApprovalPoll, s.HandlePoll) }

// HandlePoll is the core.JobHandler of core.JobApprovalPoll.
func (s *Service) HandlePoll(ctx context.Context, j core.Job) error {
	var p core.ApprovalPollPayload
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return core.Errorf(core.CodeInternal, "approval.poll payload: %w", err)
	}
	if p.Tenant == "" {
		p.Tenant = j.Tenant
	}
	_, err := s.Poll(ctx, p.Tenant)
	return err
}

// EnsurePoll queues the poll that follows the current period, once: the
// job key carries the period, so every caller in a period shares one job.
// The engine calls it at start; Open calls it for every approval it leaves
// open.
func (s *Service) EnsurePoll(ctx context.Context, tenant core.TenantID) error {
	if s.Jobs == nil {
		return nil
	}
	period := s.PollEvery
	next := s.Clock.Now().Truncate(period).Add(period)
	_, err := s.Jobs.Enqueue(ctx, core.JobSpec{
		Type: core.JobApprovalPoll, Tenant: tenant, Key: "poll@" + strconv.FormatInt(next.Unix(), 10),
		Payload: core.ApprovalPollPayload{Tenant: tenant}, At: &next,
	})
	return err //nolint:wrapcheck // port error passes through
}

// Poll looks at every open approval of the tenant: a closed tlc task (its
// own, else the run's accountable task) decides what is still undecided,
// approving or rejecting it as the person did; otherwise an expired
// approval rejects (or approves, for a human step) what is undecided with
// reason timeout. It queues the next poll while approvals stay open. One
// approval failing does not stop the others; the first error is returned
// after the whole pass.
func (s *Service) Poll(ctx context.Context, tenant core.TenantID) (PollResult, error) {
	open, err := s.Store.ListApprovals(ctx, tenant, core.ApprovalFilter{Status: core.ApprovalOpen})
	if err != nil {
		return PollResult{}, err //nolint:wrapcheck // port error passes through
	}
	res := PollResult{Open: len(open)}
	var first error
	note := func(err error) {
		if first == nil {
			first = err
		}
	}
	tasks := map[core.TaskRef]core.TaskInfo{}
	stillOpen := false
	for _, a := range open {
		switch done, how, err := s.pollOne(ctx, tenant, a, tasks); {
		case err != nil:
			note(err)
			stillOpen = true
		case done && how == "task":
			res.FromTasks++
		case done && how == "timeout":
			res.TimedOut++
		case !done:
			stillOpen = true
		}
	}
	if stillOpen {
		if err := s.EnsurePoll(ctx, tenant); err != nil {
			note(err)
		}
	}
	return res, first
}

// pollOne handles one open approval. done reports that it was completed.
func (s *Service) pollOne(ctx context.Context, tenant core.TenantID, a core.Approval, tasks map[core.TaskRef]core.TaskInfo) (done bool, how string, err error) {
	if v, by, ok, err := s.closedTask(ctx, tenant, a, tasks); err != nil {
		return false, "", err
	} else if ok {
		reason := ReasonTaskApproved
		if v == core.VerdictReject {
			reason = ReasonTaskRejected
		}
		if !allowed(a.Verdicts, v) {
			return false, "", nil // the flow does not let a person decide that way
		}
		r, err := s.Decide(ctx, tenant, core.DecideRequest{Approval: a.ID, All: v, AllReason: reason, By: by})
		return r.Approval.Complete(), "task", err
	}
	if a.ExpiresAt == nil || s.Clock.Now().Before(*a.ExpiresAt) || a.OnTimeout == OnTimeoutKeepWaiting {
		return false, "", nil
	}
	verdict := core.VerdictReject
	if a.OnTimeout == OnTimeoutApprove {
		verdict = core.VerdictApprove
	}
	r, err := s.apply(ctx, tenant, a.ID, 0, func(x *core.Approval, now time.Time) ([]string, error) {
		var decided []string
		for _, i := range x.Undecided() {
			x.Items[i].Decision = &core.ItemDecision{Verdict: verdict, By: core.DeciderEngine, At: now, Reason: core.DecisionReasonTimeout}
			decided = append(decided, x.Items[i].ID)
		}
		return decided, nil
	})
	return r.Approval.Complete(), "timeout", err
}

// closedTask reports the verdict of a person who closed the approval's task
// or the run's accountable task: done is approve, skipped is reject.
func (s *Service) closedTask(ctx context.Context, tenant core.TenantID, a core.Approval, tasks map[core.TaskRef]core.TaskInfo) (v core.Verdict, by string, ok bool, err error) {
	refs := []core.TaskRef{}
	if a.TaskRef != "" {
		refs = append(refs, core.TaskRef(a.TaskRef))
	}
	if s.Runs != nil {
		if r, err := s.Runs.GetRun(ctx, tenant, a.Run); err == nil && r.TaskRef != "" {
			refs = append(refs, core.TaskRef(r.TaskRef))
		}
	}
	for _, ref := range refs {
		info, err := s.taskInfo(ctx, tenant, ref, tasks)
		if err != nil {
			if core.IsCode(err, core.CodeTaskNotFound) {
				continue
			}
			return "", "", false, err
		}
		switch info.State {
		case core.TaskApproved:
			return core.VerdictApprove, firstNonEmpty(info.By, DeciderTLC), true, nil
		case core.TaskRejected:
			return core.VerdictReject, firstNonEmpty(info.By, DeciderTLC), true, nil
		case core.TaskOpen:
		}
	}
	return "", "", false, nil
}

// taskInfo reads a task once per pass: a run's accountable task is shared by
// its approvals.
func (s *Service) taskInfo(ctx context.Context, tenant core.TenantID, ref core.TaskRef, seen map[core.TaskRef]core.TaskInfo) (core.TaskInfo, error) {
	if info, ok := seen[ref]; ok {
		return info, nil
	}
	info, err := s.Tasks.Get(ctx, tenant, ref)
	if err != nil {
		return core.TaskInfo{}, err //nolint:wrapcheck // port error passes through
	}
	seen[ref] = info
	return info, nil
}
