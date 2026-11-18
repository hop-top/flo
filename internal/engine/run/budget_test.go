package run_test

import (
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/engine/budget"
	"hop.top/flo/spec/node"
)

func TestUSDOverrunStopsRunButLetsInFlightFinish(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	releaseC := make(chan struct{})
	h.fx.Runner.Handle(func(inv core.Invocation) bool { return inv.StepID == "a" },
		func(core.Invocation) (core.Result, error) {
			r := coretest.OK("a")
			r.Cost = core.Cost{USD: 0.6}
			return r, nil
		})
	h.fx.Runner.Handle(func(inv core.Invocation) bool { return inv.StepID == "c" },
		func(core.Invocation) (core.Result, error) { <-releaseC; return coretest.OK("c"), nil })
	id := h.start(h.flow(`flow: spend
version: 1.0.0
budget: { usd: 0.5 }
steps:
  - id: a
    uses: t/read@1
  - id: b
    uses: t/read@1
    depends_on: [a]
  - id: c
    uses: t/read@1
`), nil)
	done := h.drainAsync()
	waitFor(t, "budget event", func() bool { return contains(h.topics(id), "flo.run.budget.exceeded") })
	close(releaseC) // c was in flight when the limit was passed
	finished(t, done)

	r := h.wantRun(id, core.RunFailed, core.CodeBudgetExceeded)
	if r.Failure.Detail["limit"] != budget.LimitUSD {
		t.Errorf("failure = %+v", r.Failure)
	}
	h.wantStep(id, "a", core.StepSucceeded, "") // the step that overran stands
	h.wantStep(id, "c", core.StepSucceeded, "") // in flight: allowed to finish
	h.wantStep(id, "b", core.StepCancelled, core.ReasonBudgetExceeded)
	if n := len(h.fx.Runner.CallsFor("b")); n != 0 {
		t.Errorf("b started after the budget ran out")
	}
	if r.Usage.USD != 0.6 || r.Usage.Calls != 2 {
		t.Errorf("usage = %+v", r.Usage)
	}
	if got := h.topics(id); got[len(got)-1] != "flo.run.run.failed" {
		t.Errorf("events = %v", got)
	}
}

func TestPreCheckRefusesStepWhoseEstimateDoesNotFit(t *testing.T) {
	h := newH(t)
	h.node(&node.Spec{Node: "t/paid@1", SideEffect: node.SideEffectRead,
		Cost: &node.Cost{Class: node.CostMetered, Estimate: node.CostEstimate{USD: 0.3}}})
	h.fx.Runner.Handle(func(core.Invocation) bool { return true }, func(core.Invocation) (core.Result, error) {
		r := coretest.OK(nil)
		r.Cost = core.Cost{USD: 0.3}
		return r, nil
	})
	id := h.do(`flow: estimate
version: 1.0.0
budget: { usd: 0.5 }
steps:
  - id: a
    uses: t/paid@1
  - id: b
    uses: t/paid@1
    depends_on: [a]
`, nil)
	h.wantRun(id, core.RunFailed, core.CodeBudgetExceeded)
	h.wantStep(id, "b", core.StepCancelled, core.ReasonBudgetExceeded)
	if n := len(h.fx.Runner.Calls()); n != 1 {
		t.Errorf("calls = %d, b is refused before it starts", n)
	}
}

func TestCallsBudgetCountsInvocations(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	id := h.do(`flow: calls
version: 1.0.0
budget: { calls: 2 }
steps:
  - id: a
    uses: t/read@1
  - id: b
    uses: t/read@1
    depends_on: [a]
  - id: c
    uses: t/read@1
    depends_on: [b]
`, nil)
	h.wantRun(id, core.RunFailed, core.CodeBudgetExceeded)
	h.wantStep(id, "c", core.StepCancelled, core.ReasonBudgetExceeded)
}

func TestStepBudgetCapsItsElementsAndCountsTowardRun(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.fx.Runner.ForStep("list", coretest.OK(items("a", "b", "c")))
	id := h.do(`flow: stepcap
version: 1.0.0
budget: { calls: 100 }
steps:
  - id: list
    uses: t/read@1
  - id: each
    uses: t/read@1
    for_each: "{{ results.list.items }}"
    concurrency: 1
    budget: { calls: 2 }
`, nil)
	r := h.wantRun(id, core.RunFailed, core.CodeBudgetExceeded)
	if r.Failure.Detail["step"] != "each" {
		t.Errorf("failure = %+v, the step's own cap stopped the run", r.Failure)
	}
	if n := len(h.fx.Runner.CallsFor("each")); n != 2 {
		t.Errorf("elements run = %d, want 2 under calls: 2", n)
	}
	if r.Usage.Calls != 3 {
		t.Errorf("run usage calls = %d, the step's calls count toward the flow", r.Usage.Calls)
	}
}

func TestDurationExcludesParkedTime(t *testing.T) {
	h := newH(t)
	h.gated()
	id := h.do(`flow: parktime
version: 1.0.0
budget: { duration: 10m }
steps:
  - id: plan
    uses: t/read@1
  - id: approve
    uses: flo/approval@1
    with: { items: "{{ results.plan.items }}" }
  - id: after
    uses: t/read@1
    depends_on: [approve]
`, nil)
	h.wantRun(id, core.RunAwaitingApproval, "")
	s := h.wantStep(id, "approve", core.StepWaiting, core.ReasonApproval)
	h.fx.Clock.Advance(48 * time.Hour) // parked for two days
	h.decideAll(s.Approval, core.VerdictApprove)
	h.drain()
	r := h.wantRun(id, core.RunSucceeded, "")
	if r.Usage.Active > time.Minute {
		t.Errorf("active = %v, parked time must not count", r.Usage.Active)
	}
}

func TestDurationBudgetStopsAfterActiveTimePasses(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.fx.Runner.Handle(func(inv core.Invocation) bool { return inv.StepID == "a" },
		func(core.Invocation) (core.Result, error) {
			h.fx.Clock.Advance(11 * time.Minute) // a long step
			return coretest.OK(nil), nil
		})
	id := h.do(`flow: slowrun
version: 1.0.0
budget: { duration: 10m }
steps:
  - id: a
    uses: t/read@1
  - id: b
    uses: t/read@1
    depends_on: [a]
`, nil)
	r := h.wantRun(id, core.RunFailed, core.CodeBudgetExceeded)
	if r.Failure.Detail["limit"] != budget.LimitDuration {
		t.Errorf("failure = %+v", r.Failure)
	}
	h.wantStep(id, "b", core.StepCancelled, core.ReasonBudgetExceeded)
}
