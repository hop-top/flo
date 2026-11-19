// Package approvals is the approval service of flo (node.md §6.6, §10;
// flow.md §7.8): it opens the approvals steps park on, records decisions,
// answers the guard of irreversible steps, polls the tlc tasks and the
// timeouts, and holds the built-in `flo/approval@1` node, the core `human`
// step and the trust provider adapter.
package approvals

import (
	"context"
	"fmt"
	"strings"
	"time"

	"hop.top/kit/go/runtime/bus"

	"hop.top/flo/internal/core"
)

// DefaultPollEvery is how often open approvals are polled for a closed
// task and for a timeout.
const DefaultPollEvery = 30 * time.Second

// Deps is what the service is built from. Runs, Flows and Jobs are
// optional: without Runs there is no accountable run task and the run's
// task is not polled; without Flows every run gets a run task (the default
// of flow.md §7.8); without Jobs nothing schedules the poll (the caller
// runs Poll).
type Deps struct {
	Store   core.ApprovalStore
	Tasks   core.Tasks
	Resumer core.Resumer
	// Trust answers GuardSatisfied for a step guarded by trust.
	Trust  core.Trust
	Runs   core.RunStore
	Flows  core.FlowStore
	Jobs   core.Jobs
	RunLog core.RunLog
	Clock  core.Clock
	IDs    core.IDs
	// PollEvery is the poll period; zero is DefaultPollEvery.
	PollEvery time.Duration
}

// Service is the core.ApprovalService.
type Service struct {
	Deps
}

var _ core.ApprovalService = (*Service)(nil)

// New returns the approval service.
func New(d Deps) *Service {
	if d.PollEvery <= 0 {
		d.PollEvery = DefaultPollEvery
	}
	return &Service{Deps: d}
}

var (
	itemVerdicts  = []core.Verdict{core.VerdictApprove, core.VerdictReject, core.VerdictCorrect}
	humanVerdicts = []core.Verdict{core.VerdictApprove, core.VerdictReject}
)

// Timeout actions of an approval (ApprovalRequest.OnTimeout).
const (
	OnTimeoutReject      = "reject"
	OnTimeoutApprove     = "approve"
	OnTimeoutKeepWaiting = "keep_waiting"
)

func (s *Service) emit(ctx context.Context, topic bus.Topic, a core.Approval, item string, v core.Verdict, by, reason string) {
	if s.RunLog == nil {
		return
	}
	ev := core.NewEvent(s.Clock, s.IDs, topic, a.Tenant, a.Run, core.ApprovalEvent{
		Tenant: a.Tenant, Run: a.Run, Approval: a.ID, Step: a.StepID, Status: a.Status,
		Item: item, Verdict: v, By: by, Reason: reason, Items: len(a.Items), Waiting: len(a.Undecided()),
	})
	_ = s.RunLog.Emit(ctx, a.Tenant, a.Run, ev) // a log failure never fails the decision
}

// Open implements core.ApprovalService. It is idempotent per (run, step
// run): a second call returns the stored approval, creating its task if the
// first call died before it did.
func (s *Service) Open(ctx context.Context, tenant core.TenantID, run core.RunID, step core.StepRun, req core.ApprovalRequest) (core.Approval, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return core.Approval{}, err //nolint:wrapcheck // *core.Error
	}
	now := s.Clock.Now()
	a, err := s.build(tenant, run, step, req, now)
	if err != nil {
		return core.Approval{}, err
	}
	out, created, err := s.Store.CreateApproval(ctx, a)
	if err != nil {
		return core.Approval{}, err //nolint:wrapcheck // port error passes through
	}
	if out.Status == core.ApprovalOpen && out.Surface == core.ApprovalSurfaceTLC && out.TaskRef == "" {
		if out, err = s.openTask(ctx, out, req.Description); err != nil {
			return core.Approval{}, err
		}
	}
	if !created {
		return out, nil
	}
	s.emit(ctx, core.TopicApprovalRequested, out, "", "", "", "")
	if out.Status == core.ApprovalDecided {
		s.emit(ctx, core.TopicApprovalResolved, out, "", "", "", "")
	}
	if out.Status == core.ApprovalOpen {
		if err := s.EnsurePoll(ctx, tenant); err != nil {
			return core.Approval{}, err
		}
	}
	return out, nil
}

func (s *Service) build(tenant core.TenantID, run core.RunID, step core.StepRun, req core.ApprovalRequest, now time.Time) (core.Approval, error) {
	a := core.Approval{
		ID: core.NewApprovalID(s.IDs), Tenant: tenant, Run: run, StepRun: step.ID, StepID: step.StepID,
		Kind: req.Kind, Title: req.Title, Surface: req.Surface, Trust: req.Trust, Context: req.Context,
		Verdicts: req.Verdicts, Items: append([]core.ApprovalItem(nil), req.Items...), Assignee: req.Assignee,
		OnTimeout: req.OnTimeout, Status: core.ApprovalOpen, CreatedAt: now, UpdatedAt: now,
	}
	if a.Kind == "" {
		a.Kind = core.ApprovalItems
	}
	if a.Surface == "" {
		a.Surface = core.ApprovalSurfaceTLC
	}
	if a.Surface != core.ApprovalSurfaceTLC && a.Surface != core.ApprovalSurfaceNone {
		return core.Approval{}, core.Errorf(core.CodeInputInvalid, "approval surface %q: want tlc or none", a.Surface)
	}
	if len(a.Verdicts) == 0 {
		a.Verdicts = itemVerdicts
		if a.Kind == core.ApprovalHuman {
			a.Verdicts = humanVerdicts
		}
	}
	if a.OnTimeout == "" {
		a.OnTimeout = OnTimeoutReject
		if a.Kind == core.ApprovalHuman {
			a.OnTimeout = OnTimeoutKeepWaiting
		}
	}
	if len(a.Items) == 0 {
		return core.Approval{}, core.New(core.CodeInputInvalid, "an approval needs at least one item")
	}
	seen := map[string]bool{}
	for _, it := range a.Items {
		if it.ID == "" || seen[it.ID] {
			return core.Approval{}, core.Errorf(core.CodeInputInvalid, "approval item id %q is empty or repeated", it.ID)
		}
		seen[it.ID] = true
	}
	if req.Timeout > 0 && a.OnTimeout != OnTimeoutKeepWaiting {
		exp := now.Add(req.Timeout)
		a.ExpiresAt = &exp
	}
	if a.Complete() {
		a.Status, a.ResolvedAt = core.ApprovalDecided, &now
	}
	return a, nil
}

// openTask creates the approval's task under the run's accountable task and
// records its reference.
func (s *Service) openTask(ctx context.Context, a core.Approval, description string) (core.Approval, error) {
	parent, err := s.ensureRunTask(ctx, a.Tenant, a.Run)
	if err != nil {
		return core.Approval{}, err
	}
	title := a.Title
	if title == "" {
		title = "approve " + a.StepID
	}
	ref, err := s.Tasks.Create(ctx, a.Tenant, core.TaskSpec{
		Title: title, Description: taskDescription(a, description), Assignee: a.Assignee, Due: a.ExpiresAt,
		Parent: parent, Run: a.Run, Approval: a.ID, Tags: []string{"step:" + a.StepID},
	})
	if err != nil {
		return core.Approval{}, err //nolint:wrapcheck // port error passes through
	}
	a.TaskRef = string(ref)
	out, err := s.Store.UpdateApproval(ctx, a)
	if err != nil {
		return core.Approval{}, err //nolint:wrapcheck // port error passes through
	}
	return out, nil
}

// taskDescription is the body of an approval task: what waits, and how to
// decide it.
func taskDescription(a core.Approval, extra string) string {
	var b strings.Builder
	if extra != "" {
		b.WriteString(extra)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "run %s, step %s\n", a.Run, a.StepID)
	for _, it := range a.Items {
		fmt.Fprintf(&b, "- %s", it.ID)
		if it.Action != "" {
			fmt.Fprintf(&b, ": %s", it.Action)
		}
		if it.Why != "" {
			fmt.Fprintf(&b, " (%s)", it.Why)
		}
		if it.Decided() {
			fmt.Fprintf(&b, " [%s]", it.Decision.Verdict)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nflo approval show %s\nflo approval set %s --all=approve\nClosing this task approves what is undecided; skipping it rejects.", a.ID, a.ID)
	return b.String()
}

// Get implements core.ApprovalService.
func (s *Service) Get(ctx context.Context, tenant core.TenantID, id core.ApprovalID) (core.Approval, error) {
	return s.Store.GetApproval(ctx, tenant, id) //nolint:wrapcheck // port error passes through
}

// List implements core.ApprovalService.
func (s *Service) List(ctx context.Context, tenant core.TenantID, f core.ApprovalFilter) ([]core.Approval, error) {
	return s.Store.ListApprovals(ctx, tenant, f) //nolint:wrapcheck // port error passes through
}

// GuardSatisfied implements core.ApprovalService: one of the guarding
// approval steps has a decided approval in the run, or trust has the step's
// class at level auto.
func (s *Service) GuardSatisfied(ctx context.Context, q core.GuardQuery) (bool, error) {
	for _, step := range q.Approvals {
		as, err := s.Store.ListApprovals(ctx, q.Tenant, core.ApprovalFilter{Run: q.Run, StepID: step})
		if err != nil {
			return false, err //nolint:wrapcheck // port error passes through
		}
		for _, a := range as {
			if a.Status == core.ApprovalDecided {
				return true, nil
			}
		}
	}
	if q.TrustProvider != "" && q.TrustClass != "" && s.Trust != nil {
		levels, err := s.Trust.Levels(ctx, q.Tenant, q.TrustProvider, TrustContext(nil, q.Agent, []string{q.TrustClass}))
		if err != nil {
			return false, err //nolint:wrapcheck // port error passes through
		}
		for _, l := range levels {
			if l.Class == q.TrustClass && l.Level == core.TrustAuto {
				return true, nil
			}
		}
	}
	return false, nil
}

// CancelRun implements core.ApprovalService: every undecided item of the
// run's open approvals is rejected with reason run_cancelled and the
// approval is closed as cancelled. Nothing is reported to a trust provider
// (flow.md §11.7): reporting is the approval node's job on resume, and a
// cancelled approval is never resumed.
func (s *Service) CancelRun(ctx context.Context, tenant core.TenantID, run core.RunID) error {
	open, err := s.Store.ListApprovals(ctx, tenant, core.ApprovalFilter{Run: run, Status: core.ApprovalOpen})
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	for _, a := range open {
		if err := s.cancelOne(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) cancelOne(ctx context.Context, a core.Approval) error {
	for attempt := 0; ; attempt++ {
		now := s.Clock.Now()
		a.Items = append([]core.ApprovalItem(nil), a.Items...)
		for _, i := range a.Undecided() {
			a.Items[i].Decision = &core.ItemDecision{Verdict: core.VerdictReject, By: core.DeciderEngine, At: now, Reason: core.DecisionReasonRunCancelled}
		}
		a.Status, a.ResolvedAt = core.ApprovalCancelled, &now
		out, err := s.Store.UpdateApproval(ctx, a)
		if core.IsCode(err, core.CodeApprovalConflict) && attempt < 2 {
			if a, err = s.Store.GetApproval(ctx, a.Tenant, a.ID); err != nil {
				return err //nolint:wrapcheck // port error passes through
			}
			if a.Status != core.ApprovalOpen {
				return nil
			}
			continue
		}
		if err != nil {
			return err //nolint:wrapcheck // port error passes through
		}
		s.emit(ctx, core.TopicApprovalCancelled, out, "", "", "", core.DecisionReasonRunCancelled)
		if out.TaskRef != "" {
			_ = s.Tasks.Close(ctx, out.Tenant, core.TaskRef(out.TaskRef), core.TaskRejected, core.DecisionReasonRunCancelled)
		}
		return nil
	}
}
