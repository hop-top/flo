package run_test

import (
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/engine/run"
)

func (h *h) cancel(id core.RunID) {
	h.t.Helper()
	if _, err := h.fx.DB.Runs.RequestCancel(h.ctx, core.DefaultTenant, id); err != nil {
		h.t.Fatal(err)
	}
	if err := h.fx.Resumer.Resume(h.ctx, core.DefaultTenant, id); err != nil {
		h.t.Fatal(err)
	}
}

func TestCancelParkedRunRejectsItemsAndFinalizesCancelled(t *testing.T) {
	h := newH(t)
	h.gated()
	id := h.do(`flow: gatedstate
version: 1.0.0
state:
  k: { default: 0 }
steps:
  - id: plan
    uses: t/read@1
    outputs: { state: { k: "{{ 1 }}" } }
  - id: approve
    uses: flo/approval@1
    with: { items: "{{ results.plan.items }}" }
  - id: after
    uses: t/read@1
    depends_on: [approve]
`, nil)
	s := h.wantStep(id, "approve", core.StepWaiting, core.ReasonApproval)
	h.wantRun(id, core.RunAwaitingApproval, "")

	h.cancel(id)
	h.drain()
	r := h.wantRun(id, core.RunCancelled, core.CodeRunCancelled)
	h.wantStep(id, "approve", core.StepCancelled, core.ReasonRunCancelled)
	h.wantStep(id, "after", core.StepCancelled, core.ReasonRunCancelled)
	h.wantStep(id, "plan", core.StepSucceeded, "")
	a, _ := h.fx.Approvals.Get(h.ctx, core.DefaultTenant, s.Approval)
	if a.Status != core.ApprovalCancelled || a.Items[0].Decision == nil || a.Items[0].Decision.Reason != core.DecisionReasonRunCancelled {
		t.Errorf("approval = %+v, its undecided items are rejected as run_cancelled", a)
	}
	if got := h.snapshot("gatedstate"); len(got) != 0 {
		t.Errorf("a cancelled run committed state: %v", got)
	}
	if got := h.topics(id); got[len(got)-1] != "flo.run.run.cancelled" {
		t.Errorf("events = %v", got)
	}
	if r.FinishedAt == nil {
		t.Error("no finish time")
	}
}

func TestCancelRunningRunInterruptsInFlightSteps(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	svc := h.fx.Services()
	svc.Runner = ctxRunner{h.fx.Runner}
	h.ex = run.New(svc, run.Config{CancelPoll: 5 * time.Millisecond})
	h.ex.Register(h.fx.Worker)
	h.fx.Runner.Handle(func(inv core.Invocation) bool { return inv.StepID == "slow" },
		func(core.Invocation) (core.Result, error) { <-h.release; return core.Result{}, nil })
	id := h.start(h.flow(`flow: cancelme
version: 1.0.0
steps:
  - id: slow
    uses: t/read@1
  - id: next
    uses: t/read@1
    depends_on: [slow]
`), nil)
	done := h.drainAsync()
	h.waitCalls(&h.fx.Runner.Script, 1)
	if _, err := h.fx.DB.Runs.RequestCancel(h.ctx, core.DefaultTenant, id); err != nil {
		t.Fatal(err)
	}
	finished(t, done)
	h.wantRun(id, core.RunCancelled, core.CodeRunCancelled)
	h.wantStep(id, "slow", core.StepCancelled, core.ReasonRunCancelled)
	h.wantStep(id, "next", core.StepCancelled, core.ReasonRunCancelled)
}

func TestCancelQueuedRunNeverStarts(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	id := h.start(h.flow(`flow: never
version: 1.0.0
steps:
  - id: a
    uses: t/read@1
`), nil)
	h.cancel(id)
	h.drain()
	h.wantRun(id, core.RunCancelled, core.CodeRunCancelled)
	if n := len(h.fx.Runner.Calls()); n != 0 {
		t.Errorf("calls = %d", n)
	}
}

func TestFinishedRunPromotesQueuedRunOfItsGroup(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	name := h.flow(`flow: serial
version: 1.0.0
steps:
  - id: a
    uses: t/read@1
`)
	first, second := h.start(name, nil), h.start(name, nil)
	if h.run(second).Admitted {
		t.Fatal("the second run must wait for the group's only slot")
	}
	h.drain()
	h.wantRun(first, core.RunSucceeded, "")
	h.wantRun(second, core.RunSucceeded, "")
}

func TestCancelPreviousRunsTheNewOneAndCancelsTheParkedOne(t *testing.T) {
	h := newH(t)
	h.gated()
	name := h.flow(`flow: replace
version: 1.0.0
concurrency: { on_overlap: cancel_previous }
steps:
  - id: plan
    uses: t/read@1
  - id: approve
    uses: flo/approval@1
    with: { items: "{{ results.plan.items }}" }
`)
	first := h.start(name, nil)
	h.drain()
	h.wantRun(first, core.RunAwaitingApproval, "")
	second := h.start(name, nil)
	h.drain()
	h.wantRun(first, core.RunCancelled, core.CodeRunCancelled)
	h.wantRun(second, core.RunAwaitingApproval, "")
}
