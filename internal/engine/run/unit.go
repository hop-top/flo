package run

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"hop.top/kit/go/runtime/job"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/budget"
	"hop.top/flo/spec"
	"hop.top/flo/spec/cel"
	specrun "hop.top/flo/spec/run"
)

// unit is one step, or one element of a for_each step, being run. Its fields
// are its own: the goroutine that runs it is the only one to touch them.
type unit struct {
	t      *turn
	sp     *stepPlan
	act    cel.Activation // results as they were when the step became ready
	absent map[string]bool
	meter  *budget.Meter
	rec    core.StepRun
	index  *int
	runAs  string
}

func (t *turn) newUnit(sp *stepPlan) *unit {
	u := &unit{t: t, sp: sp, act: t.baseAct(), absent: map[string]bool{}, meter: t.meter}
	for _, d := range sp.data {
		if st := t.status[d]; st == core.StepSkipped || st == core.StepCancelled {
			u.absent[d] = true
		}
	}
	if !sp.set.Budget.IsZero() {
		u.meter = t.meter.Child(sp.step.ID, sp.set.Budget)
	}
	switch {
	case sp.set.RunAs != "":
		u.runAs = sp.set.RunAs
	case t.run.RunAs != "":
		u.runAs = t.run.RunAs
	default:
		u.runAs = t.run.StartedBy
	}
	return u
}

// element returns the unit of one for_each element.
func (u *unit) element(i int, item any) *unit {
	c := *u
	c.index = &i
	c.rec = u.t.recFor(u.sp, &core.Element{Parent: u.rec.ID, Index: i, Item: item})
	c.act = u.with(map[string]any{"item": item, "index": int64(i)})
	return &c
}

// with returns a copy of the activation with extra namespaces bound.
func (u *unit) with(extra map[string]any) cel.Activation {
	a := make(cel.Activation, len(u.act)+len(extra))
	for k, v := range u.act {
		a[k] = v
	}
	for k, v := range extra {
		a[k] = v
	}
	return a
}

func (t *turn) recFor(sp *stepPlan, el *core.Element) core.StepRun {
	if el == nil {
		if r, ok := t.recs[sp.step.ID]; ok {
			return r
		}
		return t.newRec(sp, nil)
	}
	if r, ok := t.elems[sp.step.ID][el.Index]; ok {
		return r
	}
	return t.newRec(sp, el)
}

func (u *unit) runStep(ctx context.Context) stepResult {
	if u.sp.forEach != nil {
		return u.runForEach(ctx)
	}
	u.rec = u.t.recFor(u.sp, nil)
	return u.runUnit(ctx)
}

// attemptFail is a failed attempt and what it left behind.
type attemptFail struct {
	err    *core.Error
	effect core.Effect
}

// runUnit takes a step through when, rendering, access, attempts and check.
func (u *unit) runUnit(ctx context.Context) stepResult {
	var (
		resume  *core.Resume
		reasked bool
	)
	if u.rec.Status == core.StepWaiting {
		var res stepResult
		var done bool
		if resume, reasked, res, done = u.resumeFrom(ctx); done {
			return res
		}
	} else if u.sp.when != nil {
		ok, err := u.sp.when.Eval(u.act)
		switch {
		case err != nil && u.absentRead(err):
			return u.end(core.StepSkipped, core.ReasonUpstreamSkipped, nil, nil)
		case err != nil:
			return u.fail(core.Errorf(core.CodeEvalError, "when: %w", err))
		case !ok:
			return u.end(core.StepSkipped, core.ReasonWhenFalse, nil, nil)
		}
	}
	in, inv, err := u.build()
	if err != nil {
		if u.absentRead(err) {
			return u.end(core.StepSkipped, core.ReasonUpstreamSkipped, nil, nil)
		}
		return u.fail(celError(err))
	}
	u.rec.Inputs = in
	if u.rec.Status != core.StepWaiting || u.rec.Reason == core.ReasonAdmissionDeferred {
		if res, ok := u.access(ctx, in); !ok {
			return res
		}
	}
	return u.attempts(ctx, inv, resume, reasked)
}

// absentRead reports whether err is a read of the result of a skipped or
// cancelled step, which skips the step instead of failing it (flow.md §5.7).
func (u *unit) absentRead(err error) bool {
	var ce *cel.Error
	return len(u.absent) > 0 && errors.As(err, &ce) && ce.Code == cel.CodeUnresolved
}

func celError(err error) *core.Error {
	var ce *cel.Error
	if errors.As(err, &ce) && ce.Code == cel.CodeUnresolved {
		return core.Wrap(core.CodeUnresolvedRef, err)
	}
	return core.Wrap(core.CodeEvalError, err)
}

// resumeFrom looks at a parked record and says how the step goes on. done
// means res is the answer: the decision is not there yet, or it ends the step.
func (u *unit) resumeFrom(ctx context.Context) (resume *core.Resume, reasked bool, res stepResult, done bool) {
	r := &u.rec
	switch r.Reason {
	case core.ReasonApproval, core.ReasonHuman, core.ReasonUncertain:
		a, err := u.t.e.d.ApprovalSvc.Get(ctx, u.t.run.Tenant, r.Approval)
		if err != nil {
			return nil, false, u.fail(err), true
		}
		if !a.Complete() {
			return nil, false, stepResult{status: core.StepWaiting, reason: r.Reason, waits: []waitRef{{approval: a.ID}}}, true
		}
		u.t.emitStepAs(ctx, core.TopicStepResumed, *r, 0)
		if r.Reason != core.ReasonUncertain {
			return &core.Resume{Approval: a}, false, stepResult{}, false
		}
		if d := a.Items[0].Decision; d == nil || !d.Verdict.Approving() {
			return nil, false, u.fail(core.Errorf(core.CodeUncertain, "step %s: outcome unknown and the re-run was refused", u.sp.step.ID)), true
		}
		return nil, true, stepResult{}, false
	}
	u.t.emitStepAs(ctx, core.TopicStepResumed, *r, 0)
	return nil, false, stepResult{}, false
}

// build renders the step's inputs and fills an invocation (flow.md §4.3).
func (u *unit) build() (map[string]any, core.Invocation, error) {
	run, sp := u.t.run, u.sp
	inv := core.Invocation{
		Tenant: run.Tenant,
		Run: core.RunContext{
			ID: run.ID, Flow: run.Flow.Name, Version: run.Flow.Version, StartedAt: derefTime(run.StartedAt),
		},
		StepRun: u.rec.ID, StepID: sp.step.ID, Index: u.index, Kind: sp.kind,
		RunAs: u.runAs, Principal: run.StartedBy, Timeout: sp.set.Timeout,
	}
	var in map[string]any
	switch sp.kind {
	case core.StepKindNode:
		inv.Node, inv.NodeSource = sp.node.Spec, sp.node.Source
		if sp.node.Source.Pack != "" {
			inv.Config, _ = u.t.e.cfg.Config[sp.node.Source.Pack].(map[string]any)
		}
		in = map[string]any{}
		if sp.with != nil {
			v, err := sp.with.Render(u.act)
			if err != nil {
				return nil, inv, err //nolint:wrapcheck // classified by the caller
			}
			in, _ = restoreRaw(v, sp.raw).(map[string]any)
		}
		if inv.Timeout == 0 && sp.node.Spec.Timeout != "" {
			inv.Timeout, _ = spec.ParseDuration(sp.node.Spec.Timeout)
		}
		inv.IdempotencyKey = u.idempotencyKey(in)
	case core.StepKindExec:
		v, err := sp.exec.Render(u.act)
		if err != nil {
			return nil, inv, err //nolint:wrapcheck // classified by the caller
		}
		in, _ = v.(map[string]any)
		inv.Exec = execInput(sp.step.Exec, in)
		if inv.Timeout == 0 {
			inv.Timeout = inv.Exec.Timeout
		}
	case core.StepKindHuman:
		v, err := sp.human.Render(u.act)
		if err != nil {
			return nil, inv, err //nolint:wrapcheck // classified by the caller
		}
		in, _ = v.(map[string]any)
		inv.Human = &core.HumanInput{
			Title: str(in["title"]), Description: str(in["description"]), Assignee: str(in["assignee"]),
		}
		if h := sp.step.Human; h != nil {
			inv.Human.OnTimeout = h.OnTimeout
			inv.Human.Timeout, _ = spec.ParseDuration(h.Timeout)
		}
	}
	norm, err := core.NormalizeJSON(in)
	if err != nil {
		return nil, inv, core.Wrap(core.CodeInputInvalid, err)
	}
	in, _ = norm.(map[string]any)
	inv.In = in
	u.rec.IdemKey = inv.IdempotencyKey
	return in, inv, nil
}

func execInput(e *spec.ExecSpec, in map[string]any) *core.ExecInput {
	out := &core.ExecInput{Cwd: str(in["cwd"]), StdoutMax: e.StdoutMax}
	if argv, ok := in["argv"].([]any); ok {
		for _, a := range argv {
			out.Argv = append(out.Argv, str(a))
		}
	}
	if env, ok := in["env"].(map[string]any); ok {
		out.Env = map[string]string{}
		for k, v := range env {
			out.Env[k] = str(v)
		}
	}
	if e.Timeout != "" {
		out.Timeout, _ = spec.ParseDuration(e.Timeout)
	}
	return out
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	}
	return fmt.Sprint(v)
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// idempotencyKey is the key the node declares, rendered over its inputs; a
// node without one gets the step's identity (node.md §11).
func (u *unit) idempotencyKey(in map[string]any) string {
	if k := u.sp.node.Spec.IdempotencyKey; k != "" {
		if v, err := u.inTemplate(k, in); err == nil {
			return v
		}
	}
	key := fmt.Sprintf("%s/%s", u.t.run.ID, u.sp.step.ID)
	if u.index != nil {
		key += fmt.Sprintf("/%d", *u.index)
	}
	return key
}

// repeatable reports whether executing the step twice is safe: the node is
// idempotent or names an idempotency key. A core kind is neither.
func (u *unit) repeatable() bool {
	n := u.sp.node
	return n != nil && (n.Spec.EffectiveIdempotent() || n.Spec.IdempotencyKey != "")
}

// maxAttempts is the step's retry bound, else the node's, else 1.
func (u *unit) maxAttempts() int {
	if r := u.sp.set.Retry; r != nil {
		return r.MaxAttempts
	}
	if n := u.sp.node; n != nil && n.Spec.Retry != nil && n.Spec.Retry.MaxAttempts > 0 {
		return n.Spec.Retry.MaxAttempts
	}
	return 1
}

// backoff is the sleep before attempt n+1.
func (u *unit) backoff(n int) time.Duration {
	if r := u.sp.set.Retry; r != nil && r.Backoff > 0 {
		return r.Backoff
	}
	if nd := u.sp.node; nd != nil && nd.Spec.Retry != nil && nd.Spec.Retry.Backoff != "" {
		if d, err := spec.ParseDuration(nd.Spec.Retry.Backoff); err == nil {
			return d
		}
	}
	return defaultBackoff.Compute(n - 1)
}

var defaultBackoff = job.BackoffStrategy{Initial: time.Second, Max: time.Minute, Factor: 2}

type verdict int

const (
	verdictFail verdict = iota
	verdictRetry
	verdictUncertain
)

// decide says what a failed attempt leads to (node.md §11, flow.md §7.9).
// An unknown outcome of a step that must not be repeated is uncertain; a
// failure that is transient is retried while attempts remain, and only when
// repeating is safe or the attempt is known not to have executed.
func (u *unit) decide(f *attemptFail, attempts int, reasked bool) verdict {
	repeatable := u.repeatable()
	if f.effect == core.EffectUnknown && !repeatable {
		if reasked || u.rec.Approval != "" {
			return verdictFail // a person was asked once already
		}
		return verdictUncertain
	}
	if f.err.Retryable() && attempts < u.maxAttempts() && (f.effect == core.EffectNone || repeatable) {
		return verdictRetry
	}
	return verdictFail
}

// attempts runs the dispatch, retrying per decide, then the check and the
// state staging.
func (u *unit) attempts(ctx context.Context, inv core.Invocation, resume *core.Resume, reasked bool) stepResult {
	rec := &u.rec
	var pending *attemptFail
	if rec.Status == core.StepRunning { // an attempt was in flight when the engine stopped
		pending = &attemptFail{
			err:    core.New(core.CodeInternal, "attempt interrupted before it reported").WithTransience(core.TransienceTransient),
			effect: core.EffectUnknown,
		}
		rec.Attempts = append(rec.Attempts, core.Attempt{
			N: len(rec.Attempts) + 1, StartedAt: derefTime(rec.StartedAt), FinishedAt: u.t.e.d.Clock.Now(),
			Outcome: core.OutcomeFailed, Effect: core.EffectUnknown, Failure: core.FailureOf(pending.err),
		})
	}
	for {
		if pending != nil {
			switch u.decide(pending, len(rec.Attempts), reasked) {
			case verdictUncertain:
				return u.parkUncertain(ctx, pending)
			case verdictFail:
				return u.fail(pending.err)
			}
			rec.Status = core.StepPending
			u.put(ctx)
			wait := max(u.backoff(len(rec.Attempts)), retryAfter(pending.err))
			if err := u.t.e.d.Clock.Sleep(ctx, wait); err != nil {
				return u.stopped(ctx, pending)
			}
			pending = nil
		}
		if ex := u.meter.Admit(u.estimate(), 1); ex != nil {
			u.t.noteBudget(ctx, ex)
			return u.cancelled(core.ReasonBudgetExceeded, lastFailure(rec))
		}
		n := len(rec.Attempts) + 1
		started := u.t.e.d.Clock.Now()
		if rec.StartedAt == nil {
			rec.StartedAt = &started
		}
		rec.Status = core.StepRunning
		u.put(ctx)
		if n == 1 {
			u.t.emitStep(ctx, *rec, n)
		} else {
			u.t.emitStepAs(ctx, core.TopicStepRetried, *rec, n)
		}
		inv.Attempt, inv.Resume = n, resume
		res, err := u.once(ctx, inv)
		if ctx.Err() != nil {
			return u.stopped(ctx, pending)
		}
		resume = nil
		finished := u.t.e.d.Clock.Now()
		if err != nil {
			fe := core.Wrap(core.CodeInternal, err)
			if fe.Class() == core.TransiencePermanent && core.CodeOf(err) == core.CodeInternal {
				fe = fe.WithTransience(core.TransienceTransient)
			}
			res = core.Result{Outcome: core.OutcomeFailed, Err: fe, Effect: core.EffectUnknown}
		}
		if ex := u.meter.Charge(res.Cost, res.BillableCalls()); ex != nil {
			u.t.noteBudget(ctx, ex)
		}
		at := core.Attempt{N: n, StartedAt: started, FinishedAt: finished, Outcome: res.Outcome, Effect: res.Effect, Cost: res.Cost}
		rec.Cost = rec.Cost.Add(res.Cost)
		rec.Raw, rec.Truncated, rec.Plan = res.Raw, res.Truncated, res.Plan
		switch res.Outcome {
		case core.OutcomeOK:
			rec.Attempts = append(rec.Attempts, at)
			return u.succeed(ctx, res)
		case core.OutcomePark:
			rec.Attempts = append(rec.Attempts, at)
			next, out, done := u.openApproval(ctx, res)
			if done {
				return out
			}
			resume = next
		default:
			if res.Err == nil {
				res.Err = core.New(core.CodeNodeReportedFailure, "the step failed")
			}
			at.Failure = core.FailureOf(res.Err)
			rec.Attempts = append(rec.Attempts, at)
			pending = &attemptFail{err: res.Err, effect: res.Effect}
			if res.Effect == "" {
				pending.effect = core.EffectDone
			}
		}
	}
}

func lastFailure(rec *core.StepRun) *core.Failure {
	if n := len(rec.Attempts); n > 0 {
		return rec.Attempts[n-1].Failure
	}
	return nil
}

// estimate is the node's per-call cost estimate (node.md §13).
func (u *unit) estimate() core.Cost {
	if n := u.sp.node; n != nil && n.Spec.Cost != nil {
		return core.Cost{USD: n.Spec.Cost.Estimate.USD, Tokens: n.Spec.Cost.Estimate.Tokens}
	}
	return core.Cost{}
}

// once runs one attempt through the Runner, within the worker limit and the
// per-attempt timeout. A timeout is a failure of unknown effect.
func (u *unit) once(ctx context.Context, inv core.Invocation) (core.Result, error) {
	select {
	case u.t.sem <- struct{}{}:
	case <-ctx.Done():
		return core.Result{}, ctx.Err() //nolint:wrapcheck // the caller's own cancellation
	}
	defer func() { <-u.t.sem }()
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		timedOut atomic.Bool
		wg       sync.WaitGroup
	)
	if inv.Timeout > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if u.t.e.d.Clock.Sleep(actx, inv.Timeout) == nil {
				timedOut.Store(true)
				cancel()
			}
		}()
	}
	res, err := u.t.e.d.Runner.Run(actx, inv)
	cancel()
	wg.Wait()
	if timedOut.Load() && ctx.Err() == nil && (err != nil || res.Outcome != core.OutcomeOK) {
		return core.Result{
			Outcome: core.OutcomeFailed, Effect: core.EffectUnknown, Cost: res.Cost,
			Err: core.Errorf(core.CodeTimeout, "step %s timed out after %s", u.sp.step.ID, inv.Timeout),
		}, nil
	}
	return res, err //nolint:wrapcheck // classified by the caller
}

// openApproval opens the approval a parking step asked for. When it is
// already complete (every item decided by trust) the step is dispatched again
// at once with the decisions; otherwise the step parks.
func (u *unit) openApproval(ctx context.Context, res core.Result) (*core.Resume, stepResult, bool) {
	if u.rec.Approval != "" {
		return nil, u.fail(core.Errorf(core.CodeInternal, "step %s parked again after its approval was decided", u.sp.step.ID)), true
	}
	a, err := u.t.e.d.ApprovalSvc.Open(ctx, u.t.run.Tenant, u.t.run.ID, u.rec, *res.Park)
	if err != nil {
		return nil, u.fail(err), true
	}
	u.rec.Approval = a.ID
	if a.Complete() {
		return &core.Resume{Approval: a}, stepResult{}, false
	}
	why := core.ReasonApproval
	if a.Kind == core.ApprovalHuman {
		why = core.ReasonHuman
	}
	return nil, u.park(ctx, why, waitRef{approval: a.ID}), true
}

// parkUncertain asks a person what to do with a step that may have run: the
// run parks on a decision, approve to run it again, reject to fail it.
func (u *unit) parkUncertain(ctx context.Context, f *attemptFail) stepResult {
	a, err := u.t.e.d.ApprovalSvc.Open(ctx, u.t.run.Tenant, u.t.run.ID, u.rec, core.ApprovalRequest{
		Kind:     core.ApprovalHuman,
		Title:    fmt.Sprintf("step %s may have run", u.sp.step.ID),
		Surface:  core.ApprovalSurfaceTLC,
		Verdicts: []core.Verdict{core.VerdictApprove, core.VerdictReject},
		Items: []core.ApprovalItem{{
			ID: core.HumanItemID, Action: "run the step again",
			Why: "its outcome is unknown and it is not safe to repeat: " + f.err.Message,
		}},
		Description: f.err.Message,
	})
	if err != nil {
		return u.fail(err)
	}
	u.rec.Approval, u.rec.Failure = a.ID, core.FailureOf(core.New(core.CodeUncertain, f.err.Message))
	return u.park(ctx, core.ReasonUncertain, waitRef{approval: a.ID})
}

// succeed finishes a step whose attempt produced its output: normalize it,
// check it, stage its state writes.
func (u *unit) succeed(ctx context.Context, res core.Result) stepResult {
	out, err := core.NormalizeJSON(res.Output)
	if err != nil {
		return u.fail(core.Errorf(core.CodeOutputInvalid, "step %s output is not JSON: %w", u.sp.step.ID, err))
	}
	if f := u.check(ctx, out); f != nil {
		return u.fail(f.Err())
	}
	if u.index == nil {
		if err := u.t.stage(ctx, u.sp, u.act, out); err != nil {
			return u.fail(err)
		}
	}
	return u.end(core.StepSucceeded, "", out, nil)
}

// check sends the output to the contract gateway (flow.md §5.8). A 422 fails
// the step; an unavailable gateway is retried per the step's retry, without
// running the step again.
func (u *unit) check(ctx context.Context, out any) *core.Failure {
	if u.sp.check == nil {
		return nil
	}
	v, err := u.sp.check.Render(u.with(map[string]any{"result": out}))
	if err != nil {
		return core.FailureOf(celError(err))
	}
	cv, _ := v.(map[string]any)
	req := core.CheckRequest{Contract: str(cv["contract"]), URL: str(cv["eva_url"]), Body: out}
	if req.URL == "" {
		if eva, ok := u.t.e.cfg.Config["eva"].(map[string]any); ok {
			req.URL = str(eva["url"])
		}
	}
	for n := 1; ; n++ {
		r, err := u.t.e.d.Checker.Check(ctx, req)
		switch {
		case err == nil && r.Passed:
			return nil
		case err == nil:
			return core.FailureOf(core.Errorf(core.CodeCheckFailed, "check %s: %s", req.Contract, r.Message))
		}
		ce := core.Wrap(core.CodeCheckUnavailable, err)
		if !ce.Retryable() || n >= u.maxAttempts() {
			return core.FailureOf(ce)
		}
		if u.t.e.d.Clock.Sleep(ctx, u.backoff(n)) != nil {
			return core.FailureOf(ce)
		}
	}
}

// stage evaluates outputs.state with `result` bound and stages the writes
// (flow.md §7.4).
func (t *turn) stage(ctx context.Context, sp *stepPlan, act cel.Activation, out any) error {
	if len(sp.state) == 0 {
		return nil
	}
	a := make(cel.Activation, len(act)+1)
	for k, v := range act {
		a[k] = v
	}
	a["result"] = out
	for _, k := range sortedKeys(sp.state) {
		v, err := sp.state[k].Render(a)
		if err != nil {
			return core.Errorf(core.CodeEvalError, "outputs.state.%s: %w", k, err)
		}
		nv, err := core.NormalizeJSON(v)
		if err != nil {
			return core.Wrap(core.CodeInternal, err)
		}
		if err := t.e.d.State.Stage(ctx, t.run.Tenant, t.run.ID, k, nv); err != nil {
			return err //nolint:wrapcheck // port error passes through
		}
	}
	return nil
}

// stopped ends a unit whose context was cancelled: a cancelled run cancels
// the step; a shutting-down engine leaves its record to be recovered.
func (u *unit) stopped(ctx context.Context, f *attemptFail) stepResult {
	if !u.t.stopping.Load() {
		return stepResult{status: core.StepRunning}
	}
	var fl *core.Failure
	if f != nil {
		fl = core.FailureOf(f.err)
	}
	return u.end(core.StepCancelled, core.ReasonRunCancelled, nil, fl)
}

func (u *unit) cancelled(why core.StepReason, f *core.Failure) stepResult {
	return u.end(core.StepCancelled, why, nil, f)
}

// fail ends the step as failed.
func (u *unit) fail(err error) stepResult {
	f := core.FailureOf(err)
	if u.index != nil && u.sp.set.OnItemError == specrun.OnItemErrorSkip {
		return u.end(core.StepSkipped, core.ReasonItemSkipped, nil, f)
	}
	return u.end(core.StepFailed, "", nil, f)
}

// end writes the record's terminal state and reports it.
func (u *unit) end(st core.StepStatus, why core.StepReason, out any, f *core.Failure) stepResult {
	ctx := context.WithoutCancel(u.t.stopCtx)
	now := u.t.e.d.Clock.Now()
	rec := &u.rec
	rec.Status, rec.Reason, rec.Failure, rec.FinishedAt = st, why, f, &now
	if st == core.StepSucceeded {
		rec.Output = out
	}
	u.put(ctx)
	u.t.emitStep(ctx, *rec, len(rec.Attempts))
	return stepResult{status: st, reason: why, output: out, failure: f}
}

// park records the step as waiting.
func (u *unit) park(ctx context.Context, why core.StepReason, w waitRef) stepResult {
	rec := &u.rec
	rec.Status, rec.Reason = core.StepWaiting, why
	u.put(ctx)
	u.t.emitStep(ctx, *rec, len(rec.Attempts))
	return stepResult{status: core.StepWaiting, reason: why, waits: []waitRef{w}}
}

func (u *unit) put(ctx context.Context) {
	_ = u.t.e.d.Runs.PutStep(ctx, u.t.run.Tenant, u.rec)
}

func sortedKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }

// retryAfter is the minimum delay a failure asks for (Retry-After, a
// rate-limit bucket) in Detail["retry_after_ms"].
func retryAfter(e *core.Error) time.Duration {
	var ms float64
	switch v := e.Detail["retry_after_ms"].(type) {
	case int:
		ms = float64(v)
	case int64:
		ms = float64(v)
	case float64:
		ms = v
	}
	return time.Duration(ms * float64(time.Millisecond))
}
