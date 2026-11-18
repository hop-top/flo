package run

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"hop.top/kit/go/runtime/bus"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/budget"
	"hop.top/flo/spec/cel"
	specrun "hop.top/flo/spec/run"
)

// waitRef is what a parked step waits on: an approval, or an admission
// decision of the governance backend.
type waitRef struct {
	approval core.ApprovalID // set for an approval
	receipt  core.ReceiptID  // set for a deferred admission
}

func (w waitRef) event() string {
	if w.approval != "" {
		return core.WaitingForApproval(w.approval)
	}
	return fmt.Sprintf("flo.admission.%s", w.receipt)
}

// stepResult is how a step (or element) ended. status is terminal, waiting
// (parked) or running (the engine is shutting down: nothing was recorded and
// the run is picked up again).
type stepResult struct {
	status  core.StepStatus
	reason  core.StepReason
	output  any
	failure *core.Failure
	waits   []waitRef
}

type stepDone struct {
	id  string
	res stepResult
}

// turn is one run.advance: the state of the scheduler loop. Its maps belong
// to the loop goroutine; step goroutines get copies and report on done.
type turn struct {
	e     *Executor
	run   core.Run
	p     *plan
	meter *budget.Meter
	state map[string]any

	stopCtx  context.Context
	stop     context.CancelFunc
	stopping atomic.Bool
	sem      chan struct{}
	done     chan stepDone
	wg       sync.WaitGroup

	status   map[string]core.StepStatus
	reason   map[string]core.StepReason
	failure  map[string]*core.Failure
	results  map[string]any
	waits    map[string][]waitRef
	inflight map[string]bool
	probed   map[string]bool

	// recs and elems are the step records found at turn start; read-only
	// after load so goroutines may read them.
	recs  map[string]core.StepRun
	elems map[string]map[int]core.StepRun

	mu  sync.Mutex
	hit *budget.Exceeded
}

func (e *Executor) newTurn(ctx context.Context, run core.Run, p *plan) *turn {
	sctx, stop := context.WithCancel(ctx)
	n := 1
	if p != nil {
		n += len(p.steps)
	}
	var limits specrun.Budget
	if p != nil {
		limits = p.settings.Budget
	}
	return &turn{
		e: e, run: run, p: p, meter: budget.New(e.d.Clock, limits, run.Usage),
		stopCtx: sctx, stop: stop, sem: make(chan struct{}, e.cfg.MaxParallel), done: make(chan stepDone, n),
		status: map[string]core.StepStatus{}, reason: map[string]core.StepReason{}, failure: map[string]*core.Failure{},
		results: map[string]any{}, waits: map[string][]waitRef{}, inflight: map[string]bool{}, probed: map[string]bool{},
		recs: map[string]core.StepRun{}, elems: map[string]map[int]core.StepRun{},
	}
}

func (t *turn) close() { t.stop() }

// load reads the run's step records into the turn: what is terminal stays
// terminal, a parked step stays parked, anything else starts over from its
// record.
func (t *turn) load(ctx context.Context) error {
	steps, err := t.e.d.Runs.ListSteps(ctx, t.run.Tenant, t.run.ID)
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	for _, s := range steps {
		if s.Element != nil {
			if t.elems[s.StepID] == nil {
				t.elems[s.StepID] = map[int]core.StepRun{}
			}
			t.elems[s.StepID][s.Element.Index] = s
			continue
		}
		t.recs[s.StepID] = s
		t.status[s.StepID] = s.Status
		t.reason[s.StepID] = s.Reason
		t.failure[s.StepID] = s.Failure
		switch {
		case s.Status == core.StepSucceeded:
			t.results[s.StepID] = s.Output
		case s.Status == core.StepWaiting:
			t.waits[s.StepID] = t.waitsOf(s)
		case !s.Status.Terminal():
			t.status[s.StepID] = core.StepPending
		}
	}
	return nil
}

// waitsOf lists what a loaded waiting step record (or its waiting elements)
// waits on.
func (t *turn) waitsOf(s core.StepRun) []waitRef {
	var out []waitRef
	add := func(r core.StepRun) {
		switch {
		case r.Status != core.StepWaiting:
		case r.Approval != "":
			out = append(out, waitRef{approval: r.Approval})
		case r.Reason == core.ReasonAdmissionDeferred && len(r.Receipts) > 0:
			out = append(out, waitRef{receipt: r.Receipts[len(r.Receipts)-1]})
		}
	}
	add(s)
	idx := make([]int, 0, len(t.elems[s.StepID]))
	for i := range t.elems[s.StepID] {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		add(t.elems[s.StepID][i])
	}
	return out
}

// begin moves the run to running, takes the state snapshot on the first
// turn, and loads the step records.
func (t *turn) begin(ctx context.Context) error {
	run := t.run
	first := run.StartedAt == nil
	resumed := run.Status == core.RunAwaitingApproval
	if first || resumed || run.Status != core.RunRunning {
		patch := core.RunPatch{Status: ptr(core.RunRunning)}
		if first {
			now := t.e.d.Clock.Now()
			snap, err := t.e.stateSnapshot(ctx, run, t.p)
			if err != nil {
				return err
			}
			patch.StartedAt, patch.StateSnapshot = &now, snap
		}
		if resumed {
			patch.WaitingFor, patch.ExpiresAt = ptr(""), ptr(time.Time{})
		}
		var err error
		run, err = t.e.d.Runs.UpdateRun(ctx, run.Tenant, run.ID,
			[]core.RunStatus{core.RunQueued, core.RunRunning, core.RunAwaitingApproval}, patch)
		if err != nil {
			return err //nolint:wrapcheck // port error passes through
		}
		t.run = run
		switch {
		case first:
			t.emitRun(ctx, core.TopicRunStarted, nil)
		case resumed:
			t.emitRun(ctx, core.TopicRunResumed, nil)
		}
	}
	t.state = run.StateSnapshot
	if t.state == nil {
		t.state = map[string]any{}
	}
	if err := t.load(ctx); err != nil {
		return err
	}
	if ex := t.meter.Charge(core.Cost{}, 0); ex != nil {
		t.noteBudget(ctx, ex)
	}
	return nil
}

// loop schedules until the run parks or ends.
func (t *turn) loop(ctx context.Context) error {
	var tick <-chan time.Time
	if t.e.cfg.CancelPoll > 0 {
		tk := time.NewTicker(t.e.cfg.CancelPoll)
		defer tk.Stop()
		tick = tk.C
	}
	first := true
	for {
		if ctx.Err() != nil {
			return t.shutdown(ctx)
		}
		if t.cancelRequested(ctx) {
			return t.cancelRun(ctx)
		}
		if err := t.dispatch(ctx, first); err != nil {
			return err
		}
		first = false
		if len(t.inflight) == 0 {
			switch {
			case t.budgetHit() != nil:
				return t.stopForBudget(ctx)
			case t.allTerminal():
				return t.complete(ctx)
			case t.hasWaiting():
				if parked, err := t.park(ctx); err != nil || parked {
					return err
				}
				first = true // an approval completed meanwhile: resume its step
				continue
			default:
				return t.finish(core.RunFailed, core.FailureOf(core.Errorf(core.CodeInternal, "no step can make progress")), nil)
			}
		}
		select {
		case d := <-t.done:
			t.apply(ctx, d)
		case <-tick:
		case <-ctx.Done():
		}
	}
}

func (t *turn) shutdown(ctx context.Context) error {
	t.stop()
	t.wg.Wait()
	return ctx.Err() //nolint:wrapcheck // the caller's own cancellation
}

func (t *turn) cancelRequested(ctx context.Context) bool {
	r, err := t.e.d.Runs.GetRun(ctx, t.run.Tenant, t.run.ID)
	return err == nil && r.CancelRequested
}

func (t *turn) allTerminal() bool {
	for _, sp := range t.p.steps {
		if !t.status[sp.step.ID].Terminal() {
			return false
		}
	}
	return true
}

func (t *turn) hasWaiting() bool {
	for _, st := range t.status {
		if st == core.StepWaiting {
			return true
		}
	}
	return false
}

// dispatch starts every step that can start and settles the ones that can be
// skipped, until nothing changes. checkWaiting also re-examines parked steps
// for a decision that has arrived.
func (t *turn) dispatch(ctx context.Context, checkWaiting bool) error {
	for changed := true; changed; {
		changed = false
		for _, sp := range t.p.steps {
			id := sp.step.ID
			st := t.status[id]
			switch {
			case t.inflight[id] || st.Terminal():
				continue
			case st == core.StepWaiting:
				if !checkWaiting || t.budgetHit() != nil {
					continue
				}
				ok, err := t.resumable(ctx, id)
				if err != nil {
					return err
				}
				if ok {
					t.launch(sp)
				}
				continue
			}
			if t.budgetHit() != nil {
				t.settle(ctx, sp, core.StepCancelled, core.ReasonBudgetExceeded, nil)
				changed = true
				continue
			}
			ready, failedDep := t.depState(sp)
			switch {
			case !ready:
			case failedDep:
				t.settle(ctx, sp, core.StepSkipped, core.ReasonUpstreamFailed, nil)
				changed = true
			default:
				t.launch(sp)
			}
		}
	}
	return nil
}

// depState reports whether every dependency of sp is terminal, and whether
// one of them failed (flow.md §5.4). A step skipped for an upstream failure
// carries the failure on to its own dependents.
func (t *turn) depState(sp *stepPlan) (ready, failed bool) {
	for _, d := range sp.deps {
		st, ok := t.status[d]
		if !ok || !st.Terminal() {
			return false, false
		}
		if st == core.StepFailed || (st == core.StepSkipped && t.reason[d] == core.ReasonUpstreamFailed) {
			failed = true
		}
	}
	return true, failed
}

// resumable reports whether a parked step can go on: its approval is complete
// (or an uncertain-step decision has been given), or, for a deferred
// admission, it is time to ask the backend again (once per turn).
func (t *turn) resumable(ctx context.Context, id string) (bool, error) {
	if t.probed[id] {
		return false, nil
	}
	refs := t.waits[id]
	if len(refs) == 0 {
		return true, nil
	}
	for _, w := range refs {
		if w.approval == "" {
			t.probed[id] = true
			return true, nil
		}
		a, err := t.e.d.ApprovalSvc.Get(ctx, t.run.Tenant, w.approval)
		if err != nil {
			return false, err //nolint:wrapcheck // port error passes through
		}
		if a.Complete() {
			return true, nil
		}
	}
	return false, nil
}

// launch starts a step in its own goroutine.
func (t *turn) launch(sp *stepPlan) {
	id := sp.step.ID
	t.inflight[id] = true
	u := t.newUnit(sp)
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.done <- stepDone{id: id, res: u.runStep(t.stopCtx)}
	}()
}

// apply records how a step ended.
func (t *turn) apply(ctx context.Context, d stepDone) {
	delete(t.inflight, d.id)
	r := d.res
	if r.status == core.StepRunning {
		return // interrupted by shutdown; recovered next turn
	}
	t.status[d.id], t.reason[d.id], t.failure[d.id] = r.status, r.reason, r.failure
	delete(t.waits, d.id)
	switch r.status {
	case core.StepSucceeded:
		t.results[d.id] = r.output
	case core.StepWaiting:
		t.waits[d.id] = r.waits
	}
	u := t.meter.Usage()
	_, _ = t.e.d.Runs.UpdateRun(ctx, t.run.Tenant, t.run.ID, nil, core.RunPatch{Usage: &u})
}

// settle records a step that ends without running (skipped, or cancelled
// before it started).
func (t *turn) settle(ctx context.Context, sp *stepPlan, st core.StepStatus, why core.StepReason, f *core.Failure) {
	id := sp.step.ID
	rec, ok := t.recs[id]
	if !ok {
		rec = t.newRec(sp, nil)
	}
	now := t.e.d.Clock.Now()
	rec.Status, rec.Reason, rec.Failure, rec.FinishedAt = st, why, f, &now
	_ = t.e.d.Runs.PutStep(ctx, t.run.Tenant, rec)
	t.emitStep(ctx, rec, 0)
	t.status[id], t.reason[id], t.failure[id] = st, why, f
	delete(t.waits, id)
}

func (t *turn) newRec(sp *stepPlan, el *core.Element) core.StepRun {
	now := t.e.d.Clock.Now()
	rec := core.StepRun{
		ID: core.NewStepRunID(t.e.d.IDs), Tenant: t.run.Tenant, Run: t.run.ID, StepID: sp.step.ID,
		Element: el, Kind: sp.kind, Status: core.StepPending, CreatedAt: now, UpdatedAt: now,
	}
	if sp.node != nil {
		rec.Node, rec.NodeSource = sp.node.Spec.Node, sp.node.Source
	}
	return rec
}

// park stores the run as awaiting approval. It reports false when a decision
// arrived while parking, so the loop goes on instead.
func (t *turn) park(ctx context.Context) (bool, error) {
	var refs []waitRef
	for _, sp := range t.p.steps {
		refs = append(refs, t.waits[sp.step.ID]...)
	}
	t.meter.Pause()
	usage := t.meter.Usage()
	patch := core.RunPatch{Status: ptr(core.RunAwaitingApproval), Usage: &usage}
	if len(refs) > 0 {
		patch.WaitingFor = ptr(refs[0].event())
	}
	if exp := t.earliestExpiry(ctx, refs); exp != nil {
		patch.ExpiresAt = exp
	}
	run, err := t.e.d.Runs.UpdateRun(ctx, t.run.Tenant, t.run.ID, []core.RunStatus{core.RunRunning}, patch)
	if err != nil {
		return false, err //nolint:wrapcheck // port error passes through
	}
	t.run = run
	t.emitRun(ctx, core.TopicRunParked, nil)
	arrived, err := t.decisionArrived(ctx)
	if err != nil || !arrived {
		return err == nil, err
	}
	// A decision landed between the last look and the park; its wake-up job
	// was deduplicated against this one, so carry on instead of waiting.
	back := core.RunPatch{Status: ptr(core.RunRunning), WaitingFor: ptr(""), ExpiresAt: ptr(time.Time{})}
	if t.run, err = t.e.d.Runs.UpdateRun(ctx, t.run.Tenant, t.run.ID, []core.RunStatus{core.RunAwaitingApproval}, back); err != nil {
		return false, err //nolint:wrapcheck // port error passes through
	}
	t.meter.Resume()
	t.emitRun(ctx, core.TopicRunResumed, nil)
	return false, nil
}

// decisionArrived reports whether an approval a parked step waits on is
// complete.
func (t *turn) decisionArrived(ctx context.Context) (bool, error) {
	for _, refs := range t.waits {
		for _, w := range refs {
			if w.approval == "" {
				continue
			}
			a, err := t.e.d.ApprovalSvc.Get(ctx, t.run.Tenant, w.approval)
			if err != nil {
				return false, err //nolint:wrapcheck // port error passes through
			}
			if a.Complete() {
				return true, nil
			}
		}
	}
	return false, nil
}

func (t *turn) earliestExpiry(ctx context.Context, refs []waitRef) *time.Time {
	var min *time.Time
	for _, w := range refs {
		if w.approval == "" {
			continue
		}
		a, err := t.e.d.ApprovalSvc.Get(ctx, t.run.Tenant, w.approval)
		if err != nil || a.ExpiresAt == nil || a.Complete() {
			continue
		}
		if min == nil || a.ExpiresAt.Before(*min) {
			min = a.ExpiresAt
		}
	}
	return min
}

// noteBudget records that a limit was passed: no step starts after it, the
// ones in flight finish (flow.md §7.7).
func (t *turn) noteBudget(ctx context.Context, ex *budget.Exceeded) {
	t.mu.Lock()
	first := t.hit == nil
	if first {
		t.hit = ex
	}
	t.mu.Unlock()
	if first {
		ev := core.NewEvent(t.e.d.Clock, t.e.d.IDs, core.TopicBudgetExceeded, t.run.Tenant, t.run.ID, core.BudgetEvent{
			Tenant: t.run.Tenant, Run: t.run.ID, Step: ex.Scope, Limit: ex.Limit, Max: ex.Max, Used: ex.Used,
		})
		_ = t.e.d.RunLog.Emit(ctx, t.run.Tenant, t.run.ID, ev)
	}
}

func (t *turn) budgetHit() *budget.Exceeded {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.hit
}

// stopForBudget ends a run whose budget ran out: parked steps are cancelled
// with it, the run fails with budget_exceeded.
func (t *turn) stopForBudget(ctx context.Context) error {
	t.cancelOpen(ctx, core.ReasonBudgetExceeded)
	return t.finish(core.RunFailed, t.budgetHit().Failure(), nil)
}

// cancelOpen cancels every step that has not ended, and rejects what is
// waiting for a person.
func (t *turn) cancelOpen(ctx context.Context, why core.StepReason) {
	waiting := false
	for _, sp := range t.p.steps {
		if st := t.status[sp.step.ID]; !st.Terminal() {
			waiting = waiting || st == core.StepWaiting
			t.settle(ctx, sp, core.StepCancelled, why, nil)
		}
	}
	if waiting {
		_ = t.e.d.ApprovalSvc.CancelRun(ctx, t.run.Tenant, t.run.ID)
	}
}

// cancelRun finalizes a run whose cancel was requested.
func (t *turn) cancelRun(ctx context.Context) error {
	t.stopping.Store(true)
	t.stop()
	t.wg.Wait()
	if err := t.load(ctx); err != nil {
		return err
	}
	for len(t.done) > 0 {
		t.apply(ctx, <-t.done)
	}
	if t.p != nil {
		t.cancelOpen(ctx, core.ReasonRunCancelled)
	} else {
		for id, rec := range t.recs {
			if !rec.Status.Terminal() {
				rec.Status, rec.Reason = core.StepCancelled, core.ReasonRunCancelled
				_ = t.e.d.Runs.PutStep(ctx, t.run.Tenant, rec)
				t.status[id] = core.StepCancelled
			}
		}
	}
	_ = t.e.d.ApprovalSvc.CancelRun(ctx, t.run.Tenant, t.run.ID)
	return t.finish(core.RunCancelled, core.FailureOf(core.New(core.CodeRunCancelled, "run cancelled")), nil)
}

// complete finishes a run whose steps are all terminal.
func (t *turn) complete(ctx context.Context) error {
	for _, sp := range t.p.steps {
		id := sp.step.ID
		if t.status[id] != core.StepFailed {
			continue
		}
		f := t.failure[id]
		if f == nil {
			f = core.FailureOf(core.New(core.CodeRunFailed, "step failed"))
		}
		out := &core.Failure{Code: core.CodeRunFailed, Message: fmt.Sprintf("step %s failed: %s", id, f.Message), Transience: core.TransiencePermanent,
			Detail: map[string]any{"step": id, "code": string(f.Code)}}
		if f.Code == core.CodeStateTooLarge {
			out.Code = f.Code
		}
		return t.finish(core.RunFailed, out, nil)
	}
	result, err := t.returns()
	if err != nil {
		return t.finish(core.RunFailed, core.FailureOf(err), nil)
	}
	return t.finish(core.RunSucceeded, nil, result)
}

// returns evaluates the flow's `returns` after the last step.
func (t *turn) returns() (any, error) {
	if len(t.p.returns) == 0 {
		return nil, nil //nolint:nilnil // no returns: no result
	}
	act := t.baseAct()
	keys := make([]string, 0, len(t.p.returns))
	for k := range t.p.returns {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := map[string]any{}
	for _, k := range keys {
		v, err := t.p.returns[k].Render(act)
		if err != nil {
			return nil, core.Errorf(core.CodeEvalError, "returns.%s: %w", k, err)
		}
		out[k] = v
	}
	return core.NormalizeJSON(out)
}

// finish writes the terminal status and everything that follows from it.
func (t *turn) finish(status core.RunStatus, f *core.Failure, result any) error {
	ctx := context.WithoutCancel(t.stopCtx)
	t.meter.Pause()
	run, err := t.e.d.State.FinishRun(ctx, t.run.Tenant, t.run.ID, core.RunFinish{
		Status: status, Failure: f, Result: result, Usage: t.meter.Usage(), At: t.e.d.Clock.Now(),
	})
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	topic := map[core.RunStatus]bus.Topic{
		core.RunSucceeded: core.TopicRunSucceeded, core.RunFailed: core.TopicRunFailed, core.RunCancelled: core.TopicRunCancelled,
	}[status]
	t.run = run
	t.emitRun(ctx, topic, f)
	return t.e.d.Starter.Release(ctx, run) //nolint:wrapcheck // port error passes through
}

// baseAct is the activation every expression of a step starts from: the
// namespaces bound at run start plus the results present now.
func (t *turn) baseAct() cel.Activation {
	vars := map[string]any{}
	for k, d := range t.p.flow.Vars { // declared defaults for what the caller did not bind
		if d.Default != nil {
			vars[k] = d.Default
		}
	}
	for k, v := range t.run.Vars {
		vars[k] = v
	}
	act := cel.BindVars(cel.Activation{}, vars)
	started := ""
	if t.run.StartedAt != nil {
		started = t.run.StartedAt.UTC().Format(time.RFC3339)
	}
	act["run"] = map[string]any{
		"id": string(t.run.ID), "flow": t.run.Flow.Name, "version": t.run.Flow.Version, "started_at": started,
	}
	trig := map[string]any{"kind": string(t.run.Trigger.Kind), "at": t.run.Trigger.At.UTC().Format(time.RFC3339)}
	for k, v := range t.run.Trigger.Payload {
		trig[k] = v
	}
	act["trigger"] = trig
	act["state"] = t.state
	cfg := t.e.cfg.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	act["config"] = cfg
	act["subject"] = map[string]any{}
	act["steps"] = t.p.staticSteps
	res := make(map[string]any, len(t.results))
	for k, v := range t.results {
		res[k] = v
	}
	act["results"] = res
	return act
}

func ptr[T any](v T) *T { return &v }
