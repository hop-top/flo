package run

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"hop.top/flo/internal/core"
	specrun "hop.top/flo/spec/run"
)

// runForEach runs a for_each step: one record per element, at most
// `concurrency` in flight, the step's output the list of element outputs in
// input order (flow.md §5.6).
func (u *unit) runForEach(ctx context.Context) stepResult {
	t, sp := u.t, u.sp
	u.rec = t.recFor(sp, nil)
	v, err := sp.forEach.Render(u.act)
	if err != nil {
		if u.absentRead(err) {
			return u.end(core.StepSkipped, core.ReasonUpstreamSkipped, nil, nil)
		}
		return u.fail(celError(err))
	}
	list, ok := v.([]any)
	if !ok {
		return u.fail(core.Errorf(core.CodeEvalError, "for_each: want a list, got %T", v))
	}
	norm, err := core.NormalizeJSON(list)
	if err != nil {
		return u.fail(core.Wrap(core.CodeInternal, err))
	}
	list, _ = norm.([]any)

	first := u.rec.StartedAt == nil
	if first {
		now := t.e.d.Clock.Now()
		u.rec.StartedAt = &now
	}
	u.rec.Status, u.rec.Reason = core.StepRunning, ""
	u.put(ctx)
	if first {
		t.emitStep(ctx, u.rec, 0)
	}

	units := make([]*unit, len(list))
	results := make([]stepResult, len(list))
	slots := make(chan struct{}, sp.set.ForEachConcurrency)
	var (
		wg     sync.WaitGroup
		failed atomic.Bool
	)
	for i, item := range list {
		eu := u.element(i, item)
		units[i] = eu
		if eu.rec.Status.Terminal() {
			results[i] = stepResult{status: eu.rec.Status, reason: eu.rec.Reason, output: eu.rec.Output, failure: eu.rec.Failure}
			continue
		}
		if (failed.Load() && sp.set.OnItemError == specrun.OnItemErrorFail) || ctx.Err() != nil || t.budgetHit() != nil {
			continue // never started
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			r := eu.runUnit(ctx)
			results[i] = r
			if r.status == core.StepFailed {
				failed.Store(true)
			}
		}()
	}
	wg.Wait()
	return u.aggregate(ctx, units, results)
}

// aggregate turns the element results into the step's.
func (u *unit) aggregate(ctx context.Context, units []*unit, results []stepResult) stepResult {
	var (
		outs    = []any{}
		failure *core.Failure
		waits   []waitRef
		reason  core.StepReason
		started = true
	)
	for i, r := range results {
		switch r.status {
		case core.StepSucceeded:
			outs = append(outs, r.output)
		case core.StepFailed:
			if failure == nil {
				failure = &core.Failure{
					Code: r.failure.Code, Message: fmt.Sprintf("element %d: %s", i, r.failure.Message),
					Transience: r.failure.Transience, Detail: r.failure.Detail,
				}
			}
		case core.StepWaiting:
			waits = append(waits, r.waits...)
			if reason == "" {
				reason = r.reason
			}
		case core.StepRunning:
			return stepResult{status: core.StepRunning}
		case core.StepSkipped:
		default: // cancelled, or never started
			started = false
		}
	}
	switch {
	case failure != nil:
		return u.end(core.StepFailed, "", nil, failure)
	case u.t.stopping.Load():
		return u.end(core.StepCancelled, core.ReasonRunCancelled, nil, nil)
	case u.t.budgetHit() != nil && !started:
		return u.end(core.StepCancelled, core.ReasonBudgetExceeded, nil, nil)
	case len(waits) > 0:
		u.rec.Status, u.rec.Reason = core.StepWaiting, reason
		u.put(ctx)
		u.t.emitStep(ctx, u.rec, 0)
		return stepResult{status: core.StepWaiting, reason: reason, waits: waits}
	case !started:
		return stepResult{status: core.StepRunning} // interrupted
	}
	for i, r := range results {
		if r.status != core.StepSucceeded {
			continue
		}
		if err := u.t.stage(ctx, u.sp, units[i].act, r.output); err != nil {
			return u.fail(err)
		}
	}
	return u.end(core.StepSucceeded, "", outs, nil)
}
