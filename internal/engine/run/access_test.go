package run_test

import (
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/spec/node"
)

const botFlow = `flow: acting
version: 1.0.0
run_as: bot
steps:
  - id: send
    uses: t/mail@1
`

func (h *h) mail() {
	h.node(&node.Spec{Node: "t/mail@1", SideEffect: node.SideEffectWriteShared, Capabilities: []string{"mail.send"}})
}

func TestGrantDeniedFailsStepBeforeDispatch(t *testing.T) {
	h := newH(t)
	h.mail()
	id := h.do(botFlow, nil)
	h.wantRun(id, core.RunFailed, core.CodeRunFailed)
	f := h.wantStep(id, "send", core.StepFailed, "").Failure
	if f.Code != core.CodeGrantDenied || f.Detail["missing"] == nil {
		t.Errorf("failure = %+v", f)
	}
	if n := len(h.fx.Runner.Calls()); n != 0 {
		t.Errorf("the node was dispatched %d times without its grant", n)
	}
}

func TestAdmissionAllowRecordsReceiptOnStep(t *testing.T) {
	h := newH(t)
	h.mail()
	if _, err := h.fx.Grants.Set(h.ctx, core.DefaultTenant, "bot", "mail.send"); err != nil {
		t.Fatal(err)
	}
	id := h.do(botFlow, nil)
	h.wantRun(id, core.RunSucceeded, "")
	s := h.steps(id)["send"]
	if len(s.Receipts) != 1 {
		t.Fatalf("receipts = %v", s.Receipts)
	}
	calls := h.fx.Admission.Calls()
	if len(calls) != 1 || calls[0].Agent != "bot" || calls[0].Principal != owner || calls[0].Operation != "t/mail@1" ||
		calls[0].Purpose.StepID != "send" || calls[0].SideEffect != node.SideEffectWriteShared {
		t.Errorf("admission request = %+v", calls)
	}
	if !contains(h.topics(id), "flo.run.admission.decided") {
		t.Errorf("events = %v", h.topics(id))
	}
	if got := h.fx.Runner.CallsFor("send")[0].RunAs; got != "bot" {
		t.Errorf("run_as = %q", got)
	}
}

func TestAdmissionIsSkippedForReadByTheStarter(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	id := h.do(`flow: plain
version: 1.0.0
steps:
  - id: a
    uses: t/read@1
`, nil)
	h.wantRun(id, core.RunSucceeded, "")
	if n := len(h.fx.Admission.Calls()); n != 0 {
		t.Errorf("admission asked %d times for a read step of the starter", n)
	}
}

func TestStepRunAsOtherThanStarterIsAdmittedEvenForRead(t *testing.T) {
	h := newH(t)
	h.reads("t/read@1")
	id := h.do(`flow: other
version: 1.0.0
steps:
  - id: a
    uses: t/read@1
    run_as: someone
`, nil)
	h.wantRun(id, core.RunSucceeded, "")
	if c := h.fx.Admission.Calls(); len(c) != 1 || c[0].Agent != "someone" {
		t.Errorf("admission = %+v", c)
	}
}

func TestAdmissionDeniedFailsStep(t *testing.T) {
	h := newH(t)
	h.mail()
	h.fx.Admission.Policy = func(core.AdmissionRequest) (core.AdmissionResult, string) {
		return core.AdmissionDeny, "not on the list"
	}
	id := h.do(`flow: denied
version: 1.0.0
steps:
  - id: send
    uses: t/mail@1
`, nil)
	h.wantRun(id, core.RunFailed, core.CodeRunFailed)
	f := h.wantStep(id, "send", core.StepFailed, "").Failure
	if f.Code != core.CodeAdmissionDenied {
		t.Errorf("failure = %+v", f)
	}
	if n := len(h.fx.Runner.Calls()); n != 0 {
		t.Errorf("dispatched %d times after a denial", n)
	}
}

func TestAdmissionDeferredParksUntilTheBackendDecides(t *testing.T) {
	h := newH(t)
	h.mail()
	h.fx.Admission.Policy = func(core.AdmissionRequest) (core.AdmissionResult, string) {
		return core.AdmissionDeferred, "pending review"
	}
	id := h.do(`flow: deferred
version: 1.0.0
steps:
  - id: send
    uses: t/mail@1
`, nil)
	r := h.wantRun(id, core.RunAwaitingApproval, "")
	s := h.wantStep(id, "send", core.StepWaiting, core.ReasonAdmissionDeferred)
	if len(s.Receipts) != 1 || r.WaitingFor != "flo.admission."+string(s.Receipts[0]) {
		t.Fatalf("waiting_for = %q, receipts %v", r.WaitingFor, s.Receipts)
	}
	if n := len(h.fx.Runner.Calls()); n != 0 {
		t.Fatalf("dispatched while deferred")
	}

	// still deferred: a wake-up parks again
	if err := h.fx.Resumer.Resume(h.ctx, core.DefaultTenant, id); err != nil {
		t.Fatal(err)
	}
	h.drain()
	h.wantRun(id, core.RunAwaitingApproval, "")
	h.wantStep(id, "send", core.StepWaiting, core.ReasonAdmissionDeferred)

	// the backend allows it
	if err := h.fx.Admission.Resolve(h.ctx, core.DefaultTenant, s.Receipts[0], core.AdmissionAllow, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := h.fx.Resumer.Resume(h.ctx, core.DefaultTenant, id); err != nil {
		t.Fatal(err)
	}
	h.drain()
	h.wantRun(id, core.RunSucceeded, "")
	if n := len(h.fx.Runner.Calls()); n != 1 {
		t.Errorf("calls = %d", n)
	}
}

func TestIrreversibleWithoutApprovalIsRefused(t *testing.T) {
	h := newH(t)
	h.node(&node.Spec{Node: "t/irrev@1", SideEffect: node.SideEffectIrreversible})
	id := h.do(`flow: unguarded
version: 1.0.0
steps:
  - id: go
    uses: t/irrev@1
`, nil)
	h.wantRun(id, core.RunFailed, core.CodeRunFailed)
	f := h.wantStep(id, "go", core.StepFailed, "").Failure
	if f.Code != core.CodeIrreversibleUnsafe {
		t.Errorf("failure = %+v", f)
	}
	if n := len(h.fx.Runner.Calls()); n != 0 {
		t.Errorf("an irreversible step ran %d times unguarded", n)
	}
}

func TestIrreversibleGuardedByTrustLevelAuto(t *testing.T) {
	h := newH(t)
	h.node(&node.Spec{Node: "t/irrev@1", SideEffect: node.SideEffectIrreversible,
		Trust: &node.TrustRef{Class: "{{ in.kind }}", Provider: "inbox"}})
	h.fx.Trust.SetLevels("inbox", core.TrustLevel{Class: "archive", Level: core.TrustAuto})
	h.fx.Runner.ForStep("go", coretest.OK(nil))
	id := h.do(`flow: trusted
version: 1.0.0
steps:
  - id: go
    uses: t/irrev@1
    with: { kind: archive }
`, nil)
	h.wantRun(id, core.RunSucceeded, "")
}

func TestIrreversibleAfterApprovalAncestorRuns(t *testing.T) {
	h := newH(t)
	h.gated()
	id := h.do(approvalFlow, nil)
	s := h.wantStep(id, "approve", core.StepWaiting, core.ReasonApproval)
	h.decideAll(s.Approval, core.VerdictApprove)
	h.drain()
	h.wantRun(id, core.RunSucceeded, "")
	if n := len(h.fx.Runner.CallsFor("apply")); n != 1 {
		t.Errorf("apply calls = %d", n)
	}
}

func TestIdempotencyKeyRendersOverInputs(t *testing.T) {
	h := newH(t)
	h.node(&node.Spec{Node: "t/keyed@1", SideEffect: node.SideEffectWriteShared, IdempotencyKey: "msg-{{ in.id }}"})
	id := h.do(`flow: keyed
version: 1.0.0
steps:
  - id: k
    uses: t/keyed@1
    with: { id: m7 }
`, nil)
	h.wantRun(id, core.RunSucceeded, "")
	if got := h.fx.Runner.CallsFor("k")[0].IdempotencyKey; got != "msg-m7" {
		t.Errorf("key = %q", got)
	}
}

// A step an external backend approved runs with the proposal and the
// execution key, and with the approver's input when the approval corrected the
// request.
func TestApprovedStepRunsWithProposalKeyAndCorrection(t *testing.T) {
	h := newH(t)
	h.mail()
	if _, err := h.fx.Grants.Set(h.ctx, core.DefaultTenant, "bot", "mail.send"); err != nil {
		t.Fatal(err)
	}
	h.fx.Admission.Stamp = func(r *core.Receipt) {
		r.Proposal, r.ExecutionKey, r.CorrectedInput = "agent_proposals_1", "exec_1", map[string]any{"to": "ana@example.org"}
	}
	id := h.do(botFlow, nil)
	h.wantRun(id, core.RunSucceeded, "")
	inv := h.fx.Runner.CallsFor("send")[0]
	if inv.Admission == nil || inv.Admission.Proposal != "agent_proposals_1" || inv.Admission.ExecutionKey != "exec_1" {
		t.Fatalf("admission = %+v", inv.Admission)
	}
	if inv.IdempotencyKey != "exec_1" {
		t.Errorf("idempotency key = %q, want the execution key", inv.IdempotencyKey)
	}
	if inv.In["to"] != "ana@example.org" {
		t.Errorf("input = %v, want the approver's", inv.In)
	}
	if s := h.steps(id)["send"]; s.IdemKey != "exec_1" {
		t.Errorf("step record key = %q", s.IdemKey)
	} else if in, _ := s.Inputs.(map[string]any); in["to"] != "ana@example.org" {
		t.Errorf("step record inputs = %v", s.Inputs)
	}
}

// Without an approval binding nothing is handed to the step.
func TestUnboundStepGetsNoAdmissionBinding(t *testing.T) {
	h := newH(t)
	h.mail()
	if _, err := h.fx.Grants.Set(h.ctx, core.DefaultTenant, "bot", "mail.send"); err != nil {
		t.Fatal(err)
	}
	id := h.do(botFlow, nil)
	h.wantRun(id, core.RunSucceeded, "")
	if inv := h.fx.Runner.CallsFor("send")[0]; inv.Admission != nil {
		t.Errorf("admission = %+v", inv.Admission)
	}
}
