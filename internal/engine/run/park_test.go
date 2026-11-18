package run_test

import (
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/engine/run"
	"hop.top/flo/spec/node"
)

// approvalNode scripts flo/approval@1 the way the built-in works: phase one
// parks with the request, phase two (Resume set) turns decisions into output.
func (h *h) approvalNode() {
	h.node(&node.Spec{Node: "flo/approval@1", Kind: node.KindBuiltin, SideEffect: node.SideEffectWriteLocal})
	h.fx.Runner.ForNode("flo/approval@1", func(inv core.Invocation) (core.Result, error) {
		if inv.Resume == nil {
			raw, _ := inv.In["items"].([]any)
			var items []core.ApprovalItem
			for _, r := range raw {
				m := r.(map[string]any)
				items = append(items, core.ApprovalItem{ID: m["id"].(string), Action: "act"})
			}
			return coretest.Park(core.ApprovalRequest{Kind: core.ApprovalItems, Title: "approve", Surface: "tlc", Items: items}), nil
		}
		out := map[string]any{"approved": []any{}, "rejected": []any{}}
		for _, it := range inv.Resume.Approval.Items {
			key := "rejected"
			if it.Decision.Verdict.Approving() {
				key = "approved"
			}
			out[key] = append(out[key].([]any), it.ID)
		}
		return coretest.OK(out), nil
	})
}

func (h *h) decideAll(a core.ApprovalID, v core.Verdict) {
	h.t.Helper()
	res, err := h.fx.Approvals.Decide(h.ctx, core.DefaultTenant, core.DecideRequest{Approval: a, All: v, By: "jad"})
	if err != nil || !res.Resumed {
		h.t.Fatalf("decide = %+v, %v", res, err)
	}
}

const approvalFlow = `flow: gated
version: 1.0.0
steps:
  - id: plan
    uses: t/read@1
  - id: free
    uses: t/read@1
  - id: approve
    uses: flo/approval@1
    with: { items: "{{ results.plan.items }}" }
  - id: apply
    uses: t/irrev@1
    with: { ids: "{{ results.approve.approved }}" }
`

func (h *h) gated() {
	h.reads("t/read@1")
	h.approvalNode()
	h.node(&node.Spec{Node: "t/irrev@1", SideEffect: node.SideEffectIrreversible})
	h.fx.Runner.ForStep("plan", coretest.OK(map[string]any{"items": []any{map[string]any{"id": "m1"}, map[string]any{"id": "m2"}}}))
}

func TestApprovalParksRunAndResumesWithDecisions(t *testing.T) {
	h := newH(t)
	h.gated()
	id := h.do(approvalFlow, nil)
	r := h.wantRun(id, core.RunAwaitingApproval, "")
	h.wantStep(id, "free", core.StepSucceeded, "") // independent work finishes before the park
	s := h.wantStep(id, "approve", core.StepWaiting, core.ReasonApproval)
	if _, ok := h.steps(id)["apply"]; ok {
		t.Error("a step that has not started has no record yet")
	}
	if r.WaitingFor != core.WaitingForApproval(s.Approval) || s.Approval == "" {
		t.Errorf("waiting_for = %q, approval %q", r.WaitingFor, s.Approval)
	}
	a, err := h.fx.Approvals.Get(h.ctx, core.DefaultTenant, s.Approval)
	if err != nil || len(a.Items) != 2 || a.Status != core.ApprovalOpen {
		t.Fatalf("approval = %+v, %v", a, err)
	}
	if len(h.fx.Jobs.Pending(core.JobRunAdvance)) != 0 {
		t.Error("a parked run holds no job")
	}
	if !contains(h.topics(id), "flo.run.run.parked") || !contains(h.topics(id), "flo.run.step.parked") || !contains(h.topics(id), "flo.run.approval.requested") {
		t.Errorf("events = %v", h.topics(id))
	}

	// the person approves; the run resumes without redoing what succeeded
	if _, err := h.fx.Approvals.Decide(h.ctx, core.DefaultTenant, core.DecideRequest{
		Approval: a.ID, Items: []core.ItemVerdict{{Item: "m1", Verdict: core.VerdictApprove}, {Item: "m2", Verdict: core.VerdictReject}}, By: "jad",
	}); err != nil {
		t.Fatal(err)
	}
	h.drain()
	h.wantRun(id, core.RunSucceeded, "")
	if n := len(h.fx.Runner.CallsFor("plan")); n != 1 {
		t.Errorf("plan ran %d times", n)
	}
	calls := h.fx.Runner.CallsFor("approve")
	if len(calls) != 2 || calls[0].Resume != nil || calls[1].Resume == nil || !calls[1].Resume.Approval.Complete() {
		t.Fatalf("approve calls = %+v, want park then resume with the decisions", calls)
	}
	if ids := h.fx.Runner.CallsFor("apply")[0].In["ids"]; len(ids.([]any)) != 1 || ids.([]any)[0] != "m1" {
		t.Errorf("apply got %v, only the approved item", ids)
	}
	topics := h.topics(id)
	for _, want := range []string{"flo.run.run.resumed", "flo.run.step.resumed"} {
		if !contains(topics, want) {
			t.Errorf("missing %s in %v", want, topics)
		}
	}
}

// A new engine process picks up a parked run: the steps that succeeded are
// not run again.
func TestResumeAfterRestartDoesNotRerunSucceededSteps(t *testing.T) {
	h := newH(t)
	h.gated()
	id := h.do(approvalFlow, nil)
	s := h.wantStep(id, "approve", core.StepWaiting, core.ReasonApproval)
	h.decideAll(s.Approval, core.VerdictApprove)

	svc := h.fx.Services()
	svc.Runner = ctxRunner{h.fx.Runner}
	restarted := run.New(svc, run.Config{CancelPoll: -1})
	restarted.Register(h.fx.Worker)
	h.drain()

	h.wantRun(id, core.RunSucceeded, "")
	for _, step := range []string{"plan", "free"} {
		if n := len(h.fx.Runner.CallsFor(step)); n != 1 {
			t.Errorf("%s ran %d times across the restart", step, n)
		}
	}
	if n := len(h.fx.Runner.CallsFor("apply")); n != 1 {
		t.Errorf("apply ran %d times", n)
	}
}

func TestParkWithAllItemsDecidedByTrustDoesNotPark(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	h.approvalNode()
	h.fx.Runner.ForNode("flo/approval@1", func(inv core.Invocation) (core.Result, error) {
		if inv.Resume == nil {
			dec := &core.ItemDecision{Verdict: core.VerdictAuto, By: core.DeciderTrust}
			return coretest.Park(core.ApprovalRequest{Kind: core.ApprovalItems, Items: []core.ApprovalItem{{ID: "m1", Decision: dec}}}), nil
		}
		return coretest.OK(map[string]any{"auto": []any{"m1"}}), nil
	})
	id := h.do(`flow: auto
version: 1.0.0
steps:
  - id: approve
    uses: flo/approval@1
    with: { items: [] }
`, nil)
	h.wantRun(id, core.RunSucceeded, "")
	if n := len(h.fx.Runner.CallsFor("approve")); n != 2 {
		t.Errorf("approve calls = %d", n)
	}
}

func TestHumanStepParksAndResumesWithDecision(t *testing.T) {
	h := newH(t)
	h.fx.Runner.Handle(func(inv core.Invocation) bool { return inv.StepID == "ship" },
		func(inv core.Invocation) (core.Result, error) {
			if inv.Resume != nil {
				return coretest.OK(map[string]any{"decision": "approved"}), nil
			}
			return coretest.Park(core.ApprovalRequest{
				Kind: core.ApprovalHuman, Title: "Ship?", Items: []core.ApprovalItem{{ID: core.HumanItemID, Action: "ship"}},
				Verdicts: []core.Verdict{core.VerdictApprove, core.VerdictReject},
			}), nil
		})
	id := h.do(`flow: human
version: 1.0.0
vars:
  what: { default: thing }
steps:
  - id: ship
    kind: human
    title: "Ship {{ vars.what }}?"
    human: { assignee: jad }
  - id: after
    kind: exec
    depends_on: [ship]
    exec: { argv: [true] }
`, map[string]any{"what": "it"})
	s := h.wantStep(id, "ship", core.StepWaiting, core.ReasonHuman)
	h.wantRun(id, core.RunAwaitingApproval, "")
	call := h.fx.Runner.CallsFor("ship")[0]
	if call.Kind != core.StepKindHuman || call.Human.Title != "Ship it?" || call.Human.Assignee != "jad" {
		t.Errorf("human invocation = %+v %+v", call, call.Human)
	}
	h.decideAll(s.Approval, core.VerdictApprove)
	h.drain()
	h.wantRun(id, core.RunSucceeded, "")
	if out := h.steps(id)["ship"].Output.(map[string]any); out["decision"] != "approved" {
		t.Errorf("output = %v", out)
	}
}

func TestParkedAgainAfterDecisionFailsStep(t *testing.T) {
	h := newH(t)
	h.fx.Runner.ForStep("ship", coretest.Park(core.ApprovalRequest{
		Kind: core.ApprovalHuman, Items: []core.ApprovalItem{{ID: core.HumanItemID}},
	}))
	id := h.do(`flow: loop
version: 1.0.0
steps:
  - id: ship
    kind: human
    title: Ship?
`, nil)
	s := h.wantStep(id, "ship", core.StepWaiting, core.ReasonHuman)
	h.decideAll(s.Approval, core.VerdictApprove)
	h.drain() // the script parks again: that would loop forever
	h.wantRun(id, core.RunFailed, core.CodeRunFailed)
}

func TestApprovalTimeoutDecisionResumesRun(t *testing.T) {
	// the approval poller (not the executor) decides undecided items on
	// timeout and resumes the run; the executor only sees a complete approval
	h := newH(t)
	h.gated()
	id := h.do(approvalFlow, nil)
	s := h.wantStep(id, "approve", core.StepWaiting, core.ReasonApproval)
	h.decideAll(s.Approval, core.VerdictReject)
	h.drain()
	h.wantRun(id, core.RunSucceeded, "")
	if ids := h.fx.Runner.CallsFor("apply")[0].In["ids"]; len(ids.([]any)) != 0 {
		t.Errorf("apply got %v", ids)
	}
}
