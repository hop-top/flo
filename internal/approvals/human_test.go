package approvals_test

import (
	"context"
	"testing"
	"time"

	"hop.top/flo/internal/approvals"
	"hop.top/flo/internal/core"
)

func humanInv(r *rig, h *core.HumanInput) core.Invocation {
	i := inv(r, nil)
	i.Kind, i.StepRun, i.StepID, i.Human = core.StepKindHuman, "stp_sign", "sign-off", h
	return i
}

func TestHumanParksAWholeStepDecision(t *testing.T) {
	r := newRig(t)
	res, err := approvals.Human{}.Execute(context.Background(), humanInv(r, &core.HumanInput{
		Title: "Sign off", Description: "ship it?", Assignee: "lead", Timeout: 48 * time.Hour, OnTimeout: "reject",
	}))
	if err != nil {
		t.Fatal(err)
	}
	p := res.Park
	if res.Outcome != core.OutcomePark || p == nil || p.Kind != core.ApprovalHuman || p.Title != "Sign off" || p.Description != "ship it?" ||
		p.Assignee != "lead" || p.Timeout != 48*time.Hour || p.OnTimeout != "reject" || p.Surface != "tlc" {
		t.Fatalf("result = %+v", res)
	}
	if len(p.Items) != 1 || p.Items[0].ID != core.HumanItemID || len(p.Verdicts) != 2 {
		t.Errorf("items = %+v verdicts = %v", p.Items, p.Verdicts)
	}
}

func TestHumanDefaultsKeepWaiting(t *testing.T) {
	r := newRig(t)
	res, _ := approvals.Human{}.Execute(context.Background(), humanInv(r, &core.HumanInput{}))
	if res.Park == nil || res.Park.OnTimeout != "keep_waiting" || res.Park.Title != "decide sign-off" {
		t.Errorf("park = %+v", res.Park)
	}
	res, _ = approvals.Human{}.Execute(context.Background(), humanInv(r, &core.HumanInput{OnTimeout: "shrug"}))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeInputInvalid {
		t.Errorf("bad on_timeout: %+v", res)
	}
	i := humanInv(r, nil)
	if res, _ = (approvals.Human{}).Execute(context.Background(), i); res.Outcome != core.OutcomeFailed {
		t.Errorf("no human input: %+v", res)
	}
}

// park {decision, by, at, note} through the service and a decision
func TestHumanOutputAfterDecision(t *testing.T) {
	for name, tc := range map[string]struct {
		verdict  core.Verdict
		decision string
	}{"approve": {core.VerdictApprove, "approved"}, "reject": {core.VerdictReject, "rejected"}} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.run(t, "run_1")
			res, _ := approvals.Human{}.Execute(context.Background(), humanInv(r, &core.HumanInput{Title: "Sign off"}))
			a, err := r.svc.Open(context.Background(), tenant, "run_1", step("sign-off"), *res.Park)
			if err != nil {
				t.Fatal(err)
			}
			r.decide(t, core.DecideRequest{Approval: a.ID, Items: []core.ItemVerdict{{Item: core.HumanItemID, Verdict: tc.verdict, Reason: "looks right"}}, By: "lead"})
			i := humanInv(r, &core.HumanInput{})
			i.Resume = &core.Resume{Approval: r.get(t, a.ID)}
			out, err := approvals.Human{}.Execute(context.Background(), i)
			if err != nil || out.Outcome != core.OutcomeOK {
				t.Fatalf("out = %+v %v", out, err)
			}
			o := out.Output.(map[string]any)
			if o["decision"] != tc.decision || o["by"] != "lead" || o["note"] != "looks right" || o["at"] != "2026-11-18T12:00:00Z" || len(o) != 4 {
				t.Errorf("output = %v", o)
			}
		})
	}
}

func TestHumanCorrectIsNotAllowed(t *testing.T) {
	r := newRig(t)
	r.run(t, "run_1")
	res, _ := approvals.Human{}.Execute(context.Background(), humanInv(r, &core.HumanInput{}))
	a, _ := r.svc.Open(context.Background(), tenant, "run_1", step("sign-off"), *res.Park)
	_, err := r.svc.Decide(context.Background(), tenant, core.DecideRequest{Approval: a.ID, Items: []core.ItemVerdict{{Item: core.HumanItemID, Verdict: core.VerdictCorrect}}, By: "lead"})
	if !core.IsCode(err, core.CodeInvalidDecision) {
		t.Errorf("err = %v", err)
	}
}

func TestHumanTimeoutApprove(t *testing.T) {
	r := newRig(t)
	r.run(t, "run_1")
	res, _ := approvals.Human{}.Execute(context.Background(), humanInv(r, &core.HumanInput{Timeout: time.Hour, OnTimeout: "approve"}))
	a, _ := r.svc.Open(context.Background(), tenant, "run_1", step("sign-off"), *res.Park)
	r.fx.Clock.Advance(90 * time.Minute)
	if _, err := r.svc.Poll(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	i := humanInv(r, &core.HumanInput{})
	i.Resume = &core.Resume{Approval: r.get(t, a.ID)}
	out, _ := approvals.Human{}.Execute(context.Background(), i)
	o := out.Output.(map[string]any)
	if o["decision"] != "approved" || o["by"] != "engine" || o["note"] != "timeout" {
		t.Errorf("output = %v", o)
	}
}
