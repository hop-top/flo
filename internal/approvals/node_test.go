package approvals_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"hop.top/flo/internal/access/digest"
	"hop.top/flo/internal/approvals"
	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
)

func it(id, class string) map[string]any {
	m := map[string]any{"id": id, "action": "archive", "why": "newsletter"}
	if class != "" {
		m["class"] = class
	}
	return m
}

func execute(t *testing.T, r *rig, in map[string]any) core.Result {
	t.Helper()
	res, err := r.node.Execute(context.Background(), inv(r, in))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestNodeSpec(t *testing.T) {
	sp := (&rig{}).nodeSpec(t)
	ty, err := sp.Type()
	if err != nil || ty.String() != "flo/approval@1" {
		t.Fatalf("type = %v %v", ty, err)
	}
	if sp.Kind != node.KindBuiltin || sp.SideEffect != node.SideEffectWriteShared || sp.Privacy != node.PrivacyLocal {
		t.Errorf("spec = %+v", sp)
	}
	for _, in := range []string{"items", "surface", "trust", "context", "verdicts", "title", "timeout", "on_timeout"} {
		if _, ok := sp.Inputs[in]; !ok {
			t.Errorf("no input %s", in)
		}
	}
	if !sp.Inputs["items"].Required.Flag {
		t.Error("items is not required")
	}
}

func (*rig) nodeSpec(t *testing.T) *node.Spec {
	t.Helper()
	sp, err := approvals.Spec()
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

// all-auto succeeds without a park, and records the decision
func TestNodeAllAutoSucceedsWithoutPark(t *testing.T) {
	r := newRig(t)
	r.fx.Trust.SetLevels("inbox", core.TrustLevel{Class: "archive", Level: core.TrustAuto}, core.TrustLevel{Class: "label:Finance", Level: core.TrustAuto})
	r.run(t, "run_1")
	res := execute(t, r, map[string]any{
		"items": []any{it("m1", "archive"), map[string]any{"id": "m2", "class": []any{"archive", "label:Finance"}}},
		"trust": "inbox", "context": map[string]any{"run": "run_1"},
	})
	if res.Outcome != core.OutcomeOK || res.Park != nil || res.Err != nil {
		t.Fatalf("result = %+v", res)
	}
	out := res.Output.(map[string]any)
	if len(out["auto"].([]any)) != 2 || len(out["approved"].([]any)) != 0 || len(out["rejected"].([]any)) != 0 || len(out["corrected"].([]any)) != 0 {
		t.Errorf("output = %v", out)
	}
	// the approval is recorded as decided, so a guard finds it; no person, no task, no report
	as, _ := r.svc.List(context.Background(), tenant, core.ApprovalFilter{Run: "run_1"})
	if len(as) != 1 || as[0].Status != core.ApprovalDecided || as[0].TaskRef != "" {
		t.Fatalf("approvals = %+v", as)
	}
	if ok, _ := r.svc.GuardSatisfied(context.Background(), core.GuardQuery{Tenant: tenant, Run: "run_1", StepID: "approve", Approvals: []string{"approve"}}); !ok {
		t.Error("the guard does not see an all-auto approval")
	}
	if len(r.fx.Trust.DecideCalls()) != 0 || len(r.fx.Tasks.Specs()) != 0 {
		t.Error("an all-auto approval reached trust or tlc")
	}
}

func TestNodeMixedParksWithAutoDecided(t *testing.T) {
	r := newRig(t)
	r.fx.Trust.SetLevels("inbox", core.TrustLevel{Class: "archive", Level: core.TrustAuto}, core.TrustLevel{Class: "unsubscribe", Level: core.TrustAsk})
	res := execute(t, r, map[string]any{
		"items":    []any{it("m1", "archive"), it("m2", "unsubscribe"), it("m3", ""), map[string]any{"id": "m4", "class": []any{"archive", "unsubscribe"}}},
		"trust":    "inbox",
		"verdicts": []any{"approve", "reject"},
		"timeout":  "48h", "title": "Triage", "surface": "none",
		"context": map[string]any{"run": "run_1"},
	})
	if res.Outcome != core.OutcomePark || res.Park == nil || res.Effect != core.EffectNone {
		t.Fatalf("result = %+v", res)
	}
	p := res.Park
	if p.Kind != core.ApprovalItems || p.Title != "Triage" || p.Surface != "none" || p.Trust != "inbox" || p.Timeout != 48*time.Hour ||
		p.OnTimeout != "reject" || !slices.Equal(p.Verdicts, []core.Verdict{core.VerdictApprove, core.VerdictReject}) || p.Context["run"] != "run_1" {
		t.Errorf("park = %+v", p)
	}
	auto := func(i int) bool { return p.Items[i].Decision != nil && p.Items[i].Decision.Verdict == core.VerdictAuto }
	if !auto(0) || auto(1) || auto(2) || auto(3) {
		t.Errorf("auto flags: %v %v %v %v (an item is auto when EVERY class is auto, and never without a class)", auto(0), auto(1), auto(2), auto(3))
	}
	if d := p.Items[0].Decision; d.By != core.DeciderTrust || !d.At.Equal(r.fx.Clock.Now()) {
		t.Errorf("auto decision = %+v", d)
	}
}

func TestNodeDefaultsAndItemFields(t *testing.T) {
	r := newRig(t)
	res := execute(t, r, map[string]any{"items": []any{
		map[string]any{"id": "m1", "action": "send", "why": "w", "capability": "mail.send", "args": map[string]any{"to": "a@b"}, "thread": "t9", "class": "send"},
	}})
	p := res.Park
	if p == nil || p.Surface != "tlc" || p.Title != "approve approve (triage)" || p.Timeout != 0 || p.Verdicts != nil || p.Trust != "" {
		t.Fatalf("park = %+v", p)
	}
	got := p.Items[0]
	if got.Action != "send" || got.Why != "w" || got.Capability != "mail.send" || got.Extra["thread"] != "t9" || !slices.Equal(got.Class, []string{"send"}) {
		t.Errorf("item = %+v", got)
	}
	// an item that names a capability carries the request digest of that operation (D8)
	want, _ := digest.Request{Operation: "mail.send", Scope: "default", Actor: "noor", ActingFor: "jad", Input: []byte(`{"to":"a@b"}`)}.Digest()
	if got.Digest != want {
		t.Errorf("digest = %s, want %s", got.Digest, want)
	}
}

func TestNodeRefusesBadInputs(t *testing.T) {
	r := newRig(t)
	for name, in := range map[string]map[string]any{
		"no items":         {},
		"empty items":      {"items": []any{}},
		"items not a list": {"items": "x"},
		"item not object":  {"items": []any{"x"}},
		"no id":            {"items": []any{map[string]any{"action": "x"}}},
		"repeated id":      {"items": []any{it("a", ""), it("a", "")}},
		"bad class":        {"items": []any{map[string]any{"id": "a", "class": 3}}},
		"bad surface":      {"items": []any{it("a", "")}, "surface": "slack"},
		"bad timeout":      {"items": []any{it("a", "")}, "timeout": "soon"},
		"bad on_timeout":   {"items": []any{it("a", "")}, "on_timeout": "approve"},
		"bad verdict":      {"items": []any{it("a", "")}, "verdicts": []any{"auto"}},
	} {
		res := execute(t, r, in)
		if res.Outcome != core.OutcomeFailed || res.Err == nil || res.Err.Code != core.CodeInputInvalid || res.Effect != core.EffectNone {
			t.Errorf("%s: result = %+v", name, res)
		}
	}
}

func TestNodeTrustFailureFailsTheStep(t *testing.T) {
	r := newRig(t)
	r.node = nodeWithTrust(t, r, failingTrust{})
	res := execute(t, r, map[string]any{"items": []any{it("a", "archive")}, "trust": "inbox"})
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeTimeout {
		t.Errorf("result = %+v", res)
	}
}

type failingTrust struct{ core.Trust }

func (failingTrust) Levels(context.Context, core.TenantID, string, map[string]any) ([]core.TrustLevel, error) {
	return nil, core.New(core.CodeTimeout, "provider timed out")
}

func nodeWithTrust(t *testing.T, r *rig, tr core.Trust) *approvals.Node {
	t.Helper()
	n, err := approvals.NewNode(approvals.NodeDeps{Approvals: r.svc, Store: r.fx.DB.Approvals, Trust: tr, Clock: r.fx.Clock})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// park -> open -> decide -> resume, through the real service
func parkAndOpen(t *testing.T, r *rig, trust string, ids ...string) core.Approval {
	t.Helper()
	r.run(t, "run_1")
	var list []any
	for _, id := range ids {
		list = append(list, it(id, "archive"))
	}
	in := map[string]any{"items": list}
	if trust != "" {
		in["trust"] = trust
		in["context"] = map[string]any{"run": "run_1"}
	}
	res := execute(t, r, in)
	if res.Outcome != core.OutcomePark {
		t.Fatalf("not parked: %+v", res)
	}
	a, err := r.svc.Open(context.Background(), tenant, "run_1", step("approve"), *res.Park)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func resume(t *testing.T, r *rig, id core.ApprovalID) core.Result {
	t.Helper()
	i := inv(r, nil)
	i.Resume = &core.Resume{Approval: r.get(t, id)}
	res, err := r.node.Execute(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func ids(list any) []string {
	var out []string
	for _, x := range list.([]any) {
		out = append(out, x.(map[string]any)["id"].(string))
	}
	return out
}

// each item is in exactly one output list
func TestNodeResumeSplitsItemsIntoExactlyOneList(t *testing.T) {
	r := newRig(t)
	r.fx.Trust.SetLevels("inbox", core.TrustLevel{Class: "archive", Level: core.TrustAuto})
	r.run(t, "run_1")
	in := map[string]any{"items": []any{
		it("a", "archive"), // auto
		it("b", "ask"), it("c", "ask"), it("d", "ask"), it("e", "ask"),
	}, "trust": "inbox", "context": map[string]any{}}
	res := execute(t, r, in)
	a, err := r.svc.Open(context.Background(), tenant, "run_1", step("approve"), *res.Park)
	if err != nil {
		t.Fatal(err)
	}
	r.decide(t, core.DecideRequest{Approval: a.ID, Items: []core.ItemVerdict{
		{Item: "b", Verdict: core.VerdictApprove},
		{Item: "c", Verdict: core.VerdictReject, Reason: "spam"},
		{Item: "d", Verdict: core.VerdictCorrect, Correction: map[string]any{"label": "bills"}, Reason: "wrong label"},
	}})
	// e times out
	r.fx.Clock.Advance(0)
	cur := r.get(t, a.ID)
	cur.Items[4].Decision = &core.ItemDecision{Verdict: core.VerdictReject, By: core.DeciderEngine, At: r.fx.Clock.Now(), Reason: core.DecisionReasonTimeout}
	cur.Status = core.ApprovalDecided
	if _, err := r.fx.DB.Approvals.UpdateApproval(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	out := resume(t, r, a.ID)
	if out.Outcome != core.OutcomeOK || out.Effect != core.EffectDone {
		t.Fatalf("result = %+v", out)
	}
	o := out.Output.(map[string]any)
	if got := ids(o["approved"]); !slices.Equal(got, []string{"b"}) {
		t.Errorf("approved = %v", got)
	}
	if got := ids(o["rejected"]); !slices.Equal(got, []string{"c", "e"}) {
		t.Errorf("rejected = %v", got)
	}
	if got := ids(o["corrected"]); !slices.Equal(got, []string{"d"}) {
		t.Errorf("corrected = %v", got)
	}
	if got := ids(o["auto"]); !slices.Equal(got, []string{"a"}) {
		t.Errorf("auto = %v", got)
	}
	corr := o["corrected"].([]any)[0].(map[string]any)
	if corr["correction"].(map[string]any)["label"] != "bills" {
		t.Errorf("correction = %v", corr)
	}
	reason := o["reason"].(map[string]any)
	if reason["c"] != "spam" || reason["d"] != "wrong label" || reason["e"] != "timeout" || len(reason) != 3 {
		t.Errorf("reason = %v", reason)
	}
	total := len(ids(o["approved"])) + len(ids(o["rejected"])) + len(ids(o["corrected"])) + len(ids(o["auto"]))
	if total != 5 {
		t.Errorf("%d items in the lists, want 5", total)
	}
}

// decide is called once, with the person's decisions only
func TestNodeReportsToTrustOnce(t *testing.T) {
	r := newRig(t)
	r.fx.Trust.SetLevels("inbox", core.TrustLevel{Class: "archive", Level: core.TrustAuto})
	r.run(t, "run_1")
	res := execute(t, r, map[string]any{"items": []any{it("auto1", "archive"), it("b", "ask"), it("c", "ask"), it("d", "ask")}, "trust": "inbox", "context": map[string]any{"run": "run_1"}})
	a, _ := r.svc.Open(context.Background(), tenant, "run_1", step("approve"), *res.Park)
	r.decide(t, core.DecideRequest{Approval: a.ID, Items: []core.ItemVerdict{{Item: "b", Verdict: core.VerdictApprove}, {Item: "c", Verdict: core.VerdictReject, Reason: "spam"}}})
	// d times out: an engine decision is not a person's
	cur := r.get(t, a.ID)
	cur.Items[3].Decision = &core.ItemDecision{Verdict: core.VerdictReject, By: core.DeciderEngine, At: r.fx.Clock.Now(), Reason: core.DecisionReasonTimeout}
	cur.Status = core.ApprovalDecided
	if _, err := r.fx.DB.Approvals.UpdateApproval(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	if got := resume(t, r, a.ID); got.Outcome != core.OutcomeOK {
		t.Fatalf("result = %+v", got)
	}
	calls := r.fx.Trust.DecideCalls()
	if len(calls) != 1 {
		t.Fatalf("decide called %d times", len(calls))
	}
	c := calls[0]
	if c.Provider != "inbox" || c.Params["run"] != "run_1" || len(c.Decisions) != 2 {
		t.Fatalf("call = %+v", c)
	}
	if c.Decisions[0].ID != "b" || c.Decisions[0].Verdict != core.VerdictApprove || c.Decisions[1].ID != "c" || c.Decisions[1].Reason != "spam" ||
		c.Decisions[1].Item["why"] != "newsletter" {
		t.Errorf("decisions = %+v", c.Decisions)
	}
	if !r.get(t, a.ID).Reported {
		t.Error("the approval is not marked reported")
	}
	// the executor dispatching the step again does not report again
	if got := resume(t, r, a.ID); got.Outcome != core.OutcomeOK {
		t.Fatalf("second result = %+v", got)
	}
	if n := len(r.fx.Trust.DecideCalls()); n != 1 {
		t.Errorf("decide called %d times", n)
	}
}

// any skipped entry fails the step with decision_not_recorded
func TestNodeSkippedFailsTheStep(t *testing.T) {
	r := newRig(t)
	r.fx.Trust.Skip = []string{"c"}
	a := parkAndOpen(t, r, "inbox", "b", "c")
	r.decide(t, core.DecideRequest{Approval: a.ID, All: core.VerdictApprove})
	res := resume(t, r, a.ID)
	if res.Outcome != core.OutcomeFailed || res.Err == nil || res.Err.Code != core.CodeDecisionNotRecorded {
		t.Fatalf("result = %+v", res)
	}
	if skipped, _ := res.Err.Detail["skipped"].([]string); !slices.Equal(skipped, []string{"c"}) {
		t.Errorf("detail = %v", res.Err.Detail)
	}
	// reported once: a retry does not report again
	if n := len(r.fx.Trust.DecideCalls()); n != 1 {
		t.Errorf("decide called %d times", n)
	}
	if !r.get(t, a.ID).Reported {
		t.Error("not marked reported")
	}
}

func TestNodeWithoutTrustNeverCallsIt(t *testing.T) {
	r := newRig(t)
	a := parkAndOpen(t, r, "", "b")
	r.decide(t, core.DecideRequest{Approval: a.ID, All: core.VerdictReject})
	if res := resume(t, r, a.ID); res.Outcome != core.OutcomeOK {
		t.Fatalf("result = %+v", res)
	}
	if len(r.fx.Trust.DecideCalls()) != 0 {
		t.Error("trust was called without a provider")
	}
}

// rejections caused by cancelling the run never reach decide
func TestNodeCancelledApprovalIsNotReported(t *testing.T) {
	r := newRig(t)
	a := parkAndOpen(t, r, "inbox", "b", "c")
	r.decide(t, core.DecideRequest{Approval: a.ID, Items: []core.ItemVerdict{{Item: "b", Verdict: core.VerdictApprove}}})
	if err := r.svc.CancelRun(context.Background(), tenant, "run_1"); err != nil {
		t.Fatal(err)
	}
	res := resume(t, r, a.ID)
	if res.Outcome != core.OutcomeOK {
		t.Fatalf("result = %+v", res)
	}
	if n := len(r.fx.Trust.DecideCalls()); n != 0 {
		t.Errorf("a cancelled run reported %d times to trust", n)
	}
	o := res.Output.(map[string]any)
	if !slices.Equal(ids(o["rejected"]), []string{"c"}) || o["reason"].(map[string]any)["c"] != core.DecisionReasonRunCancelled {
		t.Errorf("output = %v", o)
	}
}

func TestNodeTimeoutRejectsAreNotReported(t *testing.T) {
	r := newRig(t)
	r.run(t, "run_1")
	res := execute(t, r, map[string]any{"items": []any{it("b", "ask")}, "trust": "inbox", "timeout": "1h", "context": map[string]any{}})
	a, _ := r.svc.Open(context.Background(), tenant, "run_1", step("approve"), *res.Park)
	r.fx.Clock.Advance(2 * time.Hour)
	if _, err := r.svc.Poll(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	out := resume(t, r, a.ID)
	if out.Outcome != core.OutcomeOK || !slices.Equal(ids(out.Output.(map[string]any)["rejected"]), []string{"b"}) {
		t.Fatalf("result = %+v", out)
	}
	if n := len(r.fx.Trust.DecideCalls()); n != 0 {
		t.Errorf("a timeout reached trust (%d calls)", n)
	}
}

func TestNodeResumeBeforeCompleteIsAnEngineFault(t *testing.T) {
	r := newRig(t)
	a := parkAndOpen(t, r, "", "b")
	i := inv(r, nil)
	i.Resume = &core.Resume{Approval: r.get(t, a.ID)}
	if _, err := r.node.Execute(context.Background(), i); !core.IsCode(err, core.CodeInternal) {
		t.Errorf("err = %v", err)
	}
}
