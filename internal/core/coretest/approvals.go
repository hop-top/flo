package coretest

import (
	"hop.top/kit/go/runtime/bus"

	"context"
	"fmt"
	"sync"

	"hop.top/flo/internal/core"
)

// Tasks is the fake core.Tasks: tasks live in memory, and Complete plays the
// person closing one in the task tool.
type Tasks struct {
	mu    sync.Mutex
	n     int
	tasks map[core.TaskRef]*core.TaskInfo
	specs map[core.TaskRef]core.TaskSpec
	Clock core.Clock
}

// Create implements core.Tasks.
func (t *Tasks) Create(_ context.Context, _ core.TenantID, spec core.TaskSpec) (core.TaskRef, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tasks == nil {
		t.tasks, t.specs = map[core.TaskRef]*core.TaskInfo{}, map[core.TaskRef]core.TaskSpec{}
	}
	t.n++
	ref := core.TaskRef(fmt.Sprintf("task-%04d", t.n))
	t.tasks[ref] = &core.TaskInfo{Ref: ref, State: core.TaskOpen, Title: spec.Title}
	t.specs[ref] = clone(spec)
	return ref, nil
}

// Get implements core.Tasks.
func (t *Tasks) Get(_ context.Context, _ core.TenantID, ref core.TaskRef) (core.TaskInfo, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ti, ok := t.tasks[ref]
	if !ok {
		return core.TaskInfo{}, core.Errorf(core.CodeTaskNotFound, "task %s not found", ref)
	}
	return clone(*ti), nil
}

// Close implements core.Tasks.
func (t *Tasks) Close(_ context.Context, _ core.TenantID, ref core.TaskRef, outcome core.TaskState, note string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	ti, ok := t.tasks[ref]
	if !ok {
		return core.Errorf(core.CodeTaskNotFound, "task %s not found", ref)
	}
	if ti.State != core.TaskOpen {
		return nil
	}
	ti.State, ti.Note = outcome, note
	return nil
}

// Complete closes a task as a person would: approved or rejected.
func (t *Tasks) Complete(ref core.TaskRef, state core.TaskState, by string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ti, ok := t.tasks[ref]; ok {
		ti.State, ti.By = state, by
		if t.Clock != nil {
			at := t.Clock.Now()
			ti.At = &at
		}
	}
}

// Specs returns the specs tasks were created from, by creation order.
func (t *Tasks) Specs() []core.TaskSpec {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]core.TaskSpec, 0, len(t.specs))
	for i := 1; i <= t.n; i++ {
		out = append(out, clone(t.specs[core.TaskRef(fmt.Sprintf("task-%04d", i))]))
	}
	return out
}

// Trust is the fake core.Trust: levels set per provider, decisions recorded.
type Trust struct {
	mu        sync.Mutex
	levels    map[string][]core.TrustLevel
	decisions []TrustCall
	sets      []TrustSet
	// Skip lists decision ids Decide reports as skipped.
	Skip []string
	// SetErr is returned by Set.
	SetErr error
}

// TrustCall is one Decide call.
type TrustCall struct {
	Provider  string
	Params    map[string]any
	Decisions []core.TrustDecision
}

// TrustSet is one Set call.
type TrustSet struct {
	Provider, Class, Reason string
	Level                   core.TrustLevelName
}

// SetLevels defines what Levels returns for a provider.
func (t *Trust) SetLevels(provider string, levels ...core.TrustLevel) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.levels == nil {
		t.levels = map[string][]core.TrustLevel{}
	}
	t.levels[provider] = levels
}

// Levels implements core.Trust.
func (t *Trust) Levels(_ context.Context, _ core.TenantID, provider string, _ map[string]any) ([]core.TrustLevel, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return clone(t.levels[provider]), nil
}

// Decide implements core.Trust.
func (t *Trust) Decide(_ context.Context, _ core.TenantID, provider string, params map[string]any, decisions []core.TrustDecision) (core.TrustApplied, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.decisions = append(t.decisions, TrustCall{Provider: provider, Params: clone(params), Decisions: clone(decisions)})
	skip := map[string]bool{}
	for _, id := range t.Skip {
		skip[id] = true
	}
	var out core.TrustApplied
	for _, d := range decisions {
		if skip[d.ID] {
			out.Skipped = append(out.Skipped, d.ID)
		} else {
			out.Applied = append(out.Applied, d.ID)
		}
	}
	return out, nil
}

// Set implements core.Trust.
func (t *Trust) Set(_ context.Context, _ core.TenantID, provider, class string, level core.TrustLevelName, reason string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.SetErr != nil {
		return t.SetErr
	}
	t.sets = append(t.sets, TrustSet{Provider: provider, Class: class, Level: level, Reason: reason})
	return nil
}

// DecideCalls returns the Decide calls received, in order.
func (t *Trust) DecideCalls() []TrustCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	return clone(t.decisions)
}

// SetCalls returns the Set calls received, in order.
func (t *Trust) SetCalls() []TrustSet {
	t.mu.Lock()
	defer t.mu.Unlock()
	return clone(t.sets)
}

// ApprovalService is the fake core.ApprovalService: the rules of
// node.md §6.6 and flow.md §7.8 over the ApprovalStore, without the trust
// reporting and polling the real one adds.
type ApprovalService struct {
	Store   core.ApprovalStore
	Tasks   core.Tasks
	Resumer core.Resumer
	Trust   core.Trust
	RunLog  core.RunLog
	Clock   core.Clock
	IDs     core.IDs
}

var defaultVerdicts = []core.Verdict{core.VerdictApprove, core.VerdictReject, core.VerdictCorrect}

// Open implements core.ApprovalService.
func (s *ApprovalService) Open(ctx context.Context, tenant core.TenantID, run core.RunID, step core.StepRun, req core.ApprovalRequest) (core.Approval, error) {
	now := s.Clock.Now()
	a := core.Approval{
		ID: core.NewApprovalID(s.IDs), Tenant: tenant, Run: run, StepRun: step.ID, StepID: step.StepID,
		Kind: req.Kind, Title: req.Title, Surface: req.Surface, Trust: req.Trust, Context: clone(req.Context),
		Verdicts: req.Verdicts, Items: clone(req.Items), Assignee: req.Assignee, OnTimeout: req.OnTimeout,
		Status: core.ApprovalOpen,
	}
	if a.Kind == "" {
		a.Kind = core.ApprovalItems
	}
	if a.Surface == "" {
		a.Surface = core.ApprovalSurfaceTLC
	}
	if len(a.Verdicts) == 0 {
		a.Verdicts = defaultVerdicts
	}
	if req.Timeout > 0 {
		exp := now.Add(req.Timeout)
		a.ExpiresAt = &exp
	}
	if a.Complete() {
		a.Status, a.ResolvedAt = core.ApprovalDecided, &now
	}
	out, created, err := s.Store.CreateApproval(ctx, a)
	if err != nil {
		return core.Approval{}, err //nolint:wrapcheck // port error passes through
	}
	if !created {
		return out, nil
	}
	if out.Status == core.ApprovalOpen && out.Surface == core.ApprovalSurfaceTLC {
		ref, err := s.Tasks.Create(ctx, tenant, core.TaskSpec{Title: out.Title, Run: run, Approval: out.ID, Assignee: out.Assignee, Due: out.ExpiresAt})
		if err != nil {
			return core.Approval{}, err //nolint:wrapcheck // port error passes through
		}
		out.TaskRef = string(ref)
		if out, err = s.Store.UpdateApproval(ctx, out); err != nil {
			return core.Approval{}, err //nolint:wrapcheck // port error passes through
		}
	}
	s.emit(ctx, core.TopicApprovalRequested, out, "", "", "", "")
	return out, nil
}

func (s *ApprovalService) emit(ctx context.Context, topic bus.Topic, a core.Approval, item string, v core.Verdict, by, reason string) {
	ev := core.NewEvent(s.Clock, s.IDs, topic, a.Tenant, a.Run, core.ApprovalEvent{
		Tenant: a.Tenant, Run: a.Run, Approval: a.ID, Step: a.StepID, Status: a.Status,
		Item: item, Verdict: v, By: by, Reason: reason, Items: len(a.Items), Waiting: len(a.Undecided()),
	})
	_ = s.RunLog.Emit(ctx, a.Tenant, a.Run, ev)
}

// Get implements core.ApprovalService.
func (s *ApprovalService) Get(ctx context.Context, tenant core.TenantID, id core.ApprovalID) (core.Approval, error) {
	return s.Store.GetApproval(ctx, tenant, id) //nolint:wrapcheck // port error passes through
}

// List implements core.ApprovalService.
func (s *ApprovalService) List(ctx context.Context, tenant core.TenantID, f core.ApprovalFilter) ([]core.Approval, error) {
	return s.Store.ListApprovals(ctx, tenant, f) //nolint:wrapcheck // port error passes through
}

// Decide implements core.ApprovalService.
func (s *ApprovalService) Decide(ctx context.Context, tenant core.TenantID, req core.DecideRequest) (core.DecideResult, error) {
	a, err := s.Store.GetApproval(ctx, tenant, req.Approval)
	if err != nil {
		return core.DecideResult{}, err //nolint:wrapcheck // port error passes through
	}
	if req.Version != 0 && a.Version != req.Version {
		return core.DecideResult{}, core.Errorf(core.CodeApprovalConflict, "approval %s is at version %d, not %d", a.ID, a.Version, req.Version)
	}
	if a.Status == core.ApprovalCancelled {
		return core.DecideResult{}, core.Errorf(core.CodeApprovalAlreadyClosed, "approval %s was cancelled with its run", a.ID)
	}
	now := s.Clock.Now()
	var decided []string
	apply := func(i int, v core.ItemVerdict) error {
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
		idx := -1
		for i, it := range a.Items {
			if it.ID == iv.Item {
				idx = i
			}
		}
		if idx < 0 {
			return core.DecideResult{}, core.Errorf(core.CodeItemNotFound, "approval %s has no item %q", a.ID, iv.Item)
		}
		if err := apply(idx, iv); err != nil {
			return core.DecideResult{}, err
		}
	}
	if req.All != "" {
		for _, i := range a.Undecided() {
			if err := apply(i, core.ItemVerdict{Verdict: req.All, Reason: req.AllReason}); err != nil {
				return core.DecideResult{}, err
			}
		}
	}
	if len(decided) == 0 {
		return core.DecideResult{Approval: a}, nil
	}
	completed := a.Complete()
	if completed {
		a.Status, a.ResolvedAt = core.ApprovalDecided, &now
	}
	if a, err = s.Store.UpdateApproval(ctx, a); err != nil {
		return core.DecideResult{}, err //nolint:wrapcheck // port error passes through
	}
	for _, id := range decided {
		it, _ := a.Item(id)
		s.emit(ctx, core.TopicApprovalDecided, a, id, it.Decision.Verdict, req.By, it.Decision.Reason)
	}
	res := core.DecideResult{Approval: a, Changed: true}
	if completed {
		s.emit(ctx, core.TopicApprovalResolved, a, "", "", "", "")
		s.closeTask(ctx, a)
		if err := s.Resumer.Resume(ctx, tenant, a.Run); err != nil {
			return res, err //nolint:wrapcheck // port error passes through
		}
		res.Resumed = true
	}
	return res, nil
}

func allowed(set []core.Verdict, v core.Verdict) bool {
	if v == core.VerdictAuto || !v.Valid() {
		return false
	}
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

func (s *ApprovalService) closeTask(ctx context.Context, a core.Approval) {
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

// CancelRun implements core.ApprovalService.
func (s *ApprovalService) CancelRun(ctx context.Context, tenant core.TenantID, run core.RunID) error {
	open, err := s.Store.ListApprovals(ctx, tenant, core.ApprovalFilter{Run: run, Status: core.ApprovalOpen})
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	now := s.Clock.Now()
	for _, a := range open {
		for _, i := range a.Undecided() {
			a.Items[i].Decision = &core.ItemDecision{Verdict: core.VerdictReject, By: core.DeciderEngine, At: now, Reason: core.DecisionReasonRunCancelled}
		}
		a.Status, a.ResolvedAt = core.ApprovalCancelled, &now
		if a, err = s.Store.UpdateApproval(ctx, a); err != nil {
			return err //nolint:wrapcheck // port error passes through
		}
		s.emit(ctx, core.TopicApprovalCancelled, a, "", "", "", core.DecisionReasonRunCancelled)
		if a.TaskRef != "" {
			_ = s.Tasks.Close(ctx, tenant, core.TaskRef(a.TaskRef), core.TaskRejected, core.DecisionReasonRunCancelled)
		}
	}
	return nil
}

// GuardSatisfied implements core.ApprovalService.
func (s *ApprovalService) GuardSatisfied(ctx context.Context, q core.GuardQuery) (bool, error) {
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
	if q.TrustProvider != "" && q.TrustClass != "" {
		levels, err := s.Trust.Levels(ctx, q.Tenant, q.TrustProvider, nil)
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
