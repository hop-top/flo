package approvals_test

import (
	"context"
	"testing"
	"time"

	"hop.top/flo/internal/core"
)

func TestOpenCreatesApprovalAndTaskUnderRunTask(t *testing.T) {
	r := newRig(t)
	a := r.open(t, func(q *core.ApprovalRequest) { q.Timeout = 2 * time.Hour; q.Assignee = "jad" }, "m1", "m2")
	if a.Status != core.ApprovalOpen || a.Version == 0 || len(a.Items) != 2 || a.Kind != core.ApprovalItems {
		t.Fatalf("approval = %+v", a)
	}
	if len(a.Verdicts) != 3 || a.OnTimeout != "reject" || a.ExpiresAt == nil || !a.ExpiresAt.Equal(r.fx.Clock.Now().Add(2*time.Hour)) {
		t.Errorf("defaults = %v %q %v", a.Verdicts, a.OnTimeout, a.ExpiresAt)
	}
	if a.TaskRef == "" {
		t.Fatal("no task for surface tlc")
	}
	specs := r.fx.Tasks.Specs()
	if len(specs) != 2 {
		t.Fatalf("tasks = %d, want the run task and the approval task", len(specs))
	}
	runTask, apTask := specs[0], specs[1]
	if runTask.Title != "triage 2026-11-18T12:00:00Z" || runTask.Due == nil || !runTask.Due.Equal(r.fx.Clock.Now()) || runTask.Run != "run_1" {
		t.Errorf("run task = %+v", runTask)
	}
	if apTask.Parent != "task-0001" || apTask.Approval != a.ID || apTask.Assignee != "jad" || apTask.Title != "Approve triage" ||
		apTask.Due == nil || !apTask.Due.Equal(*a.ExpiresAt) {
		t.Errorf("approval task = %+v", apTask)
	}
	run, _ := r.fx.DB.Runs.GetRun(context.Background(), tenant, "run_1")
	if run.TaskRef != "task-0001" {
		t.Errorf("run task ref = %q", run.TaskRef)
	}
	if got := r.topics("run_1"); count(got, string(core.TopicApprovalRequested)) != 1 {
		t.Errorf("topics = %v", got)
	}
}

func TestOpenIsIdempotentAndRepairsAMissingTask(t *testing.T) {
	r := newRig(t)
	a := r.open(t, nil, "m1")
	again := r.open(t, nil, "m1")
	if again.ID != a.ID || len(r.fx.Tasks.Specs()) != 2 {
		t.Fatalf("second open: %+v, tasks %d", again, len(r.fx.Tasks.Specs()))
	}
	if got := r.topics("run_1"); count(got, string(core.TopicApprovalRequested)) != 1 {
		t.Errorf("requested twice: %v", got)
	}
	// a first call that died before the task existed: the stored approval has none
	b, err := r.svc.Open(context.Background(), tenant, "run_1", step("other"), core.ApprovalRequest{Items: items("x"), Surface: core.ApprovalSurfaceNone})
	if err != nil || b.TaskRef != "" {
		t.Fatalf("surface none: %+v %v", b, err)
	}
}

func TestOpenSurfaceNoneAndAllDecided(t *testing.T) {
	r := newRig(t)
	a := r.open(t, func(q *core.ApprovalRequest) { q.Surface = core.ApprovalSurfaceNone }, "m1")
	if a.TaskRef != "" || len(r.fx.Tasks.Specs()) != 0 {
		t.Errorf("surface none made a task: %+v", a)
	}
	r2 := newRig(t)
	b := r2.open(t, func(q *core.ApprovalRequest) {
		q.Items[0].Decision = &core.ItemDecision{Verdict: core.VerdictAuto, By: core.DeciderTrust, At: r2.fx.Clock.Now()}
	}, "m1")
	if b.Status != core.ApprovalDecided || b.ResolvedAt == nil || b.TaskRef != "" || len(r2.fx.Tasks.Specs()) != 0 {
		t.Errorf("all decided: %+v", b)
	}
	if got := r2.topics("run_1"); count(got, string(core.TopicApprovalResolved)) != 1 {
		t.Errorf("topics = %v", got)
	}
}

func TestOpenValidates(t *testing.T) {
	r := newRig(t)
	r.run(t, "run_1")
	ctx := context.Background()
	for name, req := range map[string]core.ApprovalRequest{
		"no items":    {},
		"empty id":    {Items: items("")},
		"repeated id": {Items: items("a", "a")},
		"bad surface": {Items: items("a"), Surface: "slack"},
	} {
		if _, err := r.svc.Open(ctx, tenant, "run_1", step(name), req); !core.IsCode(err, core.CodeInputInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestFlowTasksNoneMakesNoRunTask(t *testing.T) {
	r := newRig(t)
	if _, _, err := r.fx.DB.Flows.PutVersion(context.Background(), core.NewFlowVersion{
		Tenant: tenant, Name: "triage", Version: "1", Hash: "h1",
		Source: []byte("flow: triage\nversion: 1\ntasks: none\nsteps:\n  - id: sign-off\n    kind: human\n"),
	}); err != nil {
		t.Fatal(err)
	}
	a := r.open(t, nil, "m1")
	specs := r.fx.Tasks.Specs()
	if len(specs) != 1 || specs[0].Parent != "" || a.TaskRef == "" {
		t.Errorf("tasks = %+v", specs)
	}
}

func TestDecidePartialThenCompleteResumesOnce(t *testing.T) {
	r := newRig(t)
	a := r.open(t, nil, "m1", "m2")
	res := r.decide(t, core.DecideRequest{Approval: a.ID, Items: []core.ItemVerdict{{Item: "m1", Verdict: core.VerdictApprove}}})
	if !res.Changed || res.Resumed || res.Approval.Status != core.ApprovalOpen || len(res.Approval.Undecided()) != 1 {
		t.Fatalf("partial = %+v", res)
	}
	if len(r.fx.Resumer.Calls()) != 0 {
		t.Fatal("resumed with an item undecided")
	}
	res = r.decide(t, core.DecideRequest{Approval: a.ID, Items: []core.ItemVerdict{{Item: "m2", Verdict: core.VerdictReject, Reason: "spam"}}})
	if !res.Changed || !res.Resumed || res.Approval.Status != core.ApprovalDecided || res.Approval.ResolvedAt == nil {
		t.Fatalf("complete = %+v", res)
	}
	if got := r.fx.Resumer.Calls(); len(got) != 1 || got[0] != "run_1" {
		t.Fatalf("resumes = %v", got)
	}
	topics := r.topics("run_1")
	if count(topics, string(core.TopicApprovalDecided)) != 2 || count(topics, string(core.TopicApprovalResolved)) != 1 {
		t.Errorf("topics = %v", topics)
	}
	d := res.Approval.Items[1].Decision
	if d.Verdict != core.VerdictReject || d.By != "jad" || d.Reason != "spam" || !d.At.Equal(r.fx.Clock.Now()) {
		t.Errorf("decision = %+v", d)
	}
}

// the same request twice yields the same state and resumes once
func TestDecideIsIdempotent(t *testing.T) {
	r := newRig(t)
	a := r.open(t, nil, "m1", "m2")
	req := core.DecideRequest{Approval: a.ID, All: core.VerdictApprove, AllReason: "fine"}
	first := r.decide(t, req)
	r.fx.Clock.Advance(time.Minute)
	second := r.decide(t, req)
	if !first.Changed || !first.Resumed {
		t.Fatalf("first = %+v", first)
	}
	if second.Changed || second.Resumed {
		t.Fatalf("second = %+v, want no change", second)
	}
	if second.Approval.Version != first.Approval.Version {
		t.Errorf("version moved: %d -> %d", first.Approval.Version, second.Approval.Version)
	}
	if got := r.fx.Resumer.Calls(); len(got) != 1 {
		t.Fatalf("resumed %d times", len(got))
	}
	if pend := r.fx.Jobs.Pending(core.JobRunAdvance); len(pend) != 1 {
		t.Errorf("advance jobs = %d", len(pend))
	}
	// a different verdict on a decided item keeps the first decision
	third := r.decide(t, core.DecideRequest{Approval: a.ID, Items: []core.ItemVerdict{{Item: "m1", Verdict: core.VerdictReject}}})
	if third.Changed || third.Approval.Items[0].Decision.Verdict != core.VerdictApprove {
		t.Errorf("third = %+v", third)
	}
}

func TestDecideRules(t *testing.T) {
	r := newRig(t)
	a := r.open(t, func(q *core.ApprovalRequest) { q.Verdicts = []core.Verdict{core.VerdictApprove} }, "m1", "m2")
	ctx := context.Background()
	if _, err := r.svc.Decide(ctx, tenant, core.DecideRequest{Approval: a.ID, Items: []core.ItemVerdict{{Item: "m1", Verdict: core.VerdictReject}}}); !core.IsCode(err, core.CodeInvalidDecision) {
		t.Errorf("a verdict the flow does not allow: %v", err)
	}
	if _, err := r.svc.Decide(ctx, tenant, core.DecideRequest{Approval: a.ID, Items: []core.ItemVerdict{{Item: "m1", Verdict: core.VerdictAuto}}}); !core.IsCode(err, core.CodeInvalidDecision) {
		t.Errorf("auto by a person: %v", err)
	}
	if _, err := r.svc.Decide(ctx, tenant, core.DecideRequest{Approval: a.ID, Items: []core.ItemVerdict{{Item: "zz", Verdict: core.VerdictApprove}}}); !core.IsCode(err, core.CodeItemNotFound) {
		t.Errorf("unknown item: %v", err)
	}
	if _, err := r.svc.Decide(ctx, tenant, core.DecideRequest{Approval: "ap_nope", All: core.VerdictApprove}); !core.IsCode(err, core.CodeApprovalNotFound) {
		t.Errorf("unknown approval: %v", err)
	}
	if _, err := r.svc.Decide(ctx, tenant, core.DecideRequest{Approval: a.ID, All: core.VerdictApprove, Version: a.Version + 5}); !core.IsCode(err, core.CodeApprovalConflict) {
		t.Errorf("stale version: %v", err)
	}
	if got := r.get(t, a.ID); got.Version != a.Version || len(got.Undecided()) != 2 {
		t.Errorf("a refused decision changed the approval: %+v", got)
	}
}

func TestDecideCorrectionIsKept(t *testing.T) {
	r := newRig(t)
	a := r.open(t, nil, "m1")
	res := r.decide(t, core.DecideRequest{Approval: a.ID, Items: []core.ItemVerdict{{Item: "m1", Verdict: core.VerdictCorrect, Correction: map[string]any{"label": "bills"}}}})
	if d := res.Approval.Items[0].Decision; d.Verdict != core.VerdictCorrect || d.Correction.(map[string]any)["label"] != "bills" {
		t.Errorf("decision = %+v", d)
	}
}

func TestDecideClosesTheTask(t *testing.T) {
	for name, tc := range map[string]struct {
		verdict core.Verdict
		want    core.TaskState
	}{"approved": {core.VerdictApprove, core.TaskApproved}, "rejected": {core.VerdictReject, core.TaskRejected}} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			a := r.open(t, nil, "m1")
			r.decide(t, core.DecideRequest{Approval: a.ID, All: tc.verdict})
			info, err := r.fx.Tasks.Get(context.Background(), tenant, core.TaskRef(a.TaskRef))
			if err != nil || info.State != tc.want {
				t.Errorf("task = %+v %v, want %s", info, err, tc.want)
			}
		})
	}
}

func TestCancelRunRejectsUndecidedAndNeverReportsToTrust(t *testing.T) {
	r := newRig(t)
	a := r.open(t, func(q *core.ApprovalRequest) { q.Trust = "inbox" }, "m1", "m2")
	r.decide(t, core.DecideRequest{Approval: a.ID, Items: []core.ItemVerdict{{Item: "m1", Verdict: core.VerdictApprove}}})
	if err := r.svc.CancelRun(context.Background(), tenant, "run_1"); err != nil {
		t.Fatal(err)
	}
	got := r.get(t, a.ID)
	if got.Status != core.ApprovalCancelled || got.ResolvedAt == nil {
		t.Fatalf("approval = %+v", got)
	}
	m1, m2 := got.Items[0].Decision, got.Items[1].Decision
	if m1.Verdict != core.VerdictApprove || m1.By != "jad" {
		t.Errorf("a decided item changed: %+v", m1)
	}
	if m2.Verdict != core.VerdictReject || m2.Reason != core.DecisionReasonRunCancelled || m2.By != core.DeciderEngine {
		t.Errorf("m2 = %+v", m2)
	}
	if n := len(r.fx.Trust.DecideCalls()); n != 0 {
		t.Errorf("a cancellation reached the trust provider (%d calls)", n)
	}
	if len(r.fx.Resumer.Calls()) != 0 {
		t.Error("a cancelled approval resumed the run")
	}
	if info, _ := r.fx.Tasks.Get(context.Background(), tenant, core.TaskRef(a.TaskRef)); info.State != core.TaskRejected || info.Note != core.DecisionReasonRunCancelled {
		t.Errorf("task = %+v", info)
	}
	if got := r.topics("run_1"); count(got, string(core.TopicApprovalCancelled)) != 1 {
		t.Errorf("topics = %v", got)
	}
	if _, err := r.svc.Decide(context.Background(), tenant, core.DecideRequest{Approval: a.ID, All: core.VerdictApprove, By: "jad"}); !core.IsCode(err, core.CodeApprovalAlreadyClosed) {
		t.Errorf("deciding a cancelled approval: %v", err)
	}
	// cancelling again, and a run with nothing open, are no-ops
	if err := r.svc.CancelRun(context.Background(), tenant, "run_1"); err != nil {
		t.Errorf("second cancel: %v", err)
	}
}

func TestGuardSatisfied(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	q := core.GuardQuery{Tenant: tenant, Run: "run_1", StepID: "send", Approvals: []string{"approve"}}
	if ok, err := r.svc.GuardSatisfied(ctx, q); err != nil || ok {
		t.Fatalf("no approval: %v %v", ok, err)
	}
	a := r.open(t, nil, "m1")
	if ok, _ := r.svc.GuardSatisfied(ctx, q); ok {
		t.Error("an open approval satisfies the guard")
	}
	r.decide(t, core.DecideRequest{Approval: a.ID, All: core.VerdictApprove})
	if ok, _ := r.svc.GuardSatisfied(ctx, q); !ok {
		t.Error("a decided approval does not satisfy the guard")
	}
	if ok, _ := r.svc.GuardSatisfied(ctx, core.GuardQuery{Tenant: tenant, Run: "run_1", StepID: "send", Approvals: []string{"other"}}); ok {
		t.Error("the approval of another step satisfies the guard")
	}
	// trust at level auto guards the step without an approval
	tq := core.GuardQuery{Tenant: tenant, Run: "run_2", StepID: "send", TrustProvider: "inbox", TrustClass: "archive"}
	r.fx.Trust.SetLevels("inbox", core.TrustLevel{Class: "archive", Level: core.TrustAsk})
	if ok, _ := r.svc.GuardSatisfied(ctx, tq); ok {
		t.Error("ask satisfies the guard")
	}
	r.fx.Trust.SetLevels("inbox", core.TrustLevel{Class: "archive", Level: core.TrustAuto})
	if ok, _ := r.svc.GuardSatisfied(ctx, tq); !ok {
		t.Error("auto does not satisfy the guard")
	}
	// a cancelled approval is not a decision
	r2 := newRig(t)
	b := r2.open(t, nil, "m1")
	if err := r2.svc.CancelRun(ctx, tenant, "run_1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r2.svc.GuardSatisfied(ctx, q); ok {
		t.Errorf("a cancelled approval %s satisfies the guard", b.ID)
	}
}

func TestListAndGet(t *testing.T) {
	r := newRig(t)
	a := r.open(t, nil, "m1")
	got, err := r.svc.List(context.Background(), tenant, core.ApprovalFilter{Status: core.ApprovalOpen})
	if err != nil || len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("list = %v %v", got, err)
	}
	if _, err := r.svc.Get(context.Background(), tenant, "ap_nope"); !core.IsCode(err, core.CodeApprovalNotFound) {
		t.Errorf("get: %v", err)
	}
}
