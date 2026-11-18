package approvals_test

import (
	"context"
	"testing"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/tasks/tlccli"
)

func pollOnce(t *testing.T, r *rig) {
	t.Helper()
	if _, err := r.svc.Poll(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
}

func TestPollTaskApprovedApprovesWhatIsUndecided(t *testing.T) {
	r := newRig(t)
	a := r.open(t, nil, "m1", "m2")
	r.decide(t, core.DecideRequest{Approval: a.ID, Items: []core.ItemVerdict{{Item: "m1", Verdict: core.VerdictReject, Reason: "no"}}})
	r.fx.Tasks.Complete(core.TaskRef(a.TaskRef), core.TaskApproved, "lead")
	res, err := r.svc.Poll(context.Background(), tenant)
	if err != nil || res.FromTasks != 1 || res.Open != 1 {
		t.Fatalf("poll = %+v %v", res, err)
	}
	got := r.get(t, a.ID)
	if got.Status != core.ApprovalDecided {
		t.Fatalf("status = %s", got.Status)
	}
	if d := got.Items[0].Decision; d.Verdict != core.VerdictReject || d.By != "jad" {
		t.Errorf("a decided item changed: %+v", d)
	}
	if d := got.Items[1].Decision; d.Verdict != core.VerdictApprove || d.By != "lead" || d.Reason != "task approved" {
		t.Errorf("m2 = %+v", d)
	}
	if n := len(r.fx.Resumer.Calls()); n != 1 {
		t.Errorf("resumes = %d", n)
	}
	// a second poll finds nothing open
	res, err = r.svc.Poll(context.Background(), tenant)
	if err != nil || res.Open != 0 || len(r.fx.Resumer.Calls()) != 1 {
		t.Errorf("second poll = %+v %v", res, err)
	}
}

func TestPollTaskSkippedRejects(t *testing.T) {
	r := newRig(t)
	a := r.open(t, nil, "m1", "m2")
	r.fx.Tasks.Complete(core.TaskRef(a.TaskRef), core.TaskRejected, "")
	pollOnce(t, r)
	got := r.get(t, a.ID)
	for _, it := range got.Items {
		if it.Decision == nil || it.Decision.Verdict != core.VerdictReject || it.Decision.By != "tlc" || it.Decision.Reason != "task rejected" {
			t.Errorf("item %s = %+v", it.ID, it.Decision)
		}
	}
	if got.Status != core.ApprovalDecided {
		t.Errorf("status = %s", got.Status)
	}
}

// Approving the accountable (run) task approves what is undecided in the
// run's approvals (flow.md §7.8).
func TestPollRunTaskApproves(t *testing.T) {
	r := newRig(t)
	a := r.open(t, nil, "m1")
	run, _ := r.fx.DB.Runs.GetRun(context.Background(), tenant, "run_1")
	r.fx.Tasks.Complete(core.TaskRef(run.TaskRef), core.TaskApproved, "jad")
	pollOnce(t, r)
	if got := r.get(t, a.ID); got.Status != core.ApprovalDecided || got.Items[0].Decision.Verdict != core.VerdictApprove {
		t.Errorf("approval = %+v", got)
	}
}

func TestPollLeavesAnOpenTaskAlone(t *testing.T) {
	r := newRig(t)
	a := r.open(t, nil, "m1")
	pollOnce(t, r)
	if got := r.get(t, a.ID); got.Status != core.ApprovalOpen || got.Version != a.Version {
		t.Errorf("approval = %+v", got)
	}
}

func TestPollTaskThatIsGoneIsSkipped(t *testing.T) {
	r := newRig(t)
	a := r.open(t, func(q *core.ApprovalRequest) { q.Timeout = time.Hour }, "m1")
	// the approval points at a task tlc no longer has
	a.TaskRef = "task-9999"
	if _, err := r.fx.DB.Approvals.UpdateApproval(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Poll(context.Background(), tenant); err != nil {
		t.Fatalf("a missing task failed the poll: %v", err)
	}
}

func TestPollTimeoutRejectsUndecided(t *testing.T) {
	r := newRig(t)
	a := r.open(t, func(q *core.ApprovalRequest) { q.Timeout = time.Hour }, "m1", "m2")
	r.decide(t, core.DecideRequest{Approval: a.ID, Items: []core.ItemVerdict{{Item: "m1", Verdict: core.VerdictApprove}}})
	r.fx.Clock.Advance(59 * time.Minute)
	if res, _ := r.svc.Poll(context.Background(), tenant); res.TimedOut != 0 {
		t.Fatal("timed out early")
	}
	r.fx.Clock.Advance(2 * time.Minute)
	res, err := r.svc.Poll(context.Background(), tenant)
	if err != nil || res.TimedOut != 1 {
		t.Fatalf("poll = %+v %v", res, err)
	}
	got := r.get(t, a.ID)
	if got.Status != core.ApprovalDecided {
		t.Fatalf("status = %s", got.Status)
	}
	if d := got.Items[0].Decision; d.Verdict != core.VerdictApprove {
		t.Errorf("m1 = %+v", d)
	}
	d := got.Items[1].Decision
	if d.Verdict != core.VerdictReject || d.By != core.DeciderEngine || d.Reason != core.DecisionReasonTimeout {
		t.Errorf("m2 = %+v", d)
	}
	if n := len(r.fx.Resumer.Calls()); n != 1 {
		t.Errorf("resumes = %d", n)
	}
	info, _ := r.fx.Tasks.Get(context.Background(), tenant, core.TaskRef(a.TaskRef))
	if info.State != core.TaskApproved { // m1 was approved, so the guarded step may act
		t.Errorf("task = %+v", info)
	}
}

func TestPollTimeoutKeepWaitingAndApprove(t *testing.T) {
	r := newRig(t)
	a := r.open(t, func(q *core.ApprovalRequest) { q.Timeout = time.Hour; q.OnTimeout = "keep_waiting" }, "m1")
	if a.ExpiresAt != nil {
		t.Errorf("keep_waiting has an expiry: %v", a.ExpiresAt)
	}
	r.fx.Clock.Advance(48 * time.Hour)
	pollOnce(t, r)
	if got := r.get(t, a.ID); got.Status != core.ApprovalOpen {
		t.Errorf("keep_waiting decided: %s", got.Status)
	}

	r2 := newRig(t)
	b := r2.open(t, func(q *core.ApprovalRequest) {
		q.Timeout = time.Hour
		q.OnTimeout = "approve"
		q.Kind = core.ApprovalHuman
		q.Verdicts = []core.Verdict{core.VerdictApprove, core.VerdictReject}
		q.Items = []core.ApprovalItem{{ID: core.HumanItemID}}
	})
	r2.fx.Clock.Advance(2 * time.Hour)
	pollOnce(t, r2)
	if d := r2.get(t, b.ID).Items[0].Decision; d == nil || d.Verdict != core.VerdictApprove || d.By != core.DeciderEngine || d.Reason != "timeout" {
		t.Errorf("human timeout decision = %+v", d)
	}
}

func TestPollSchedulesItselfWhileApprovalsStayOpen(t *testing.T) {
	r := newRig(t)
	a := r.open(t, nil, "m1")
	pend := r.fx.Jobs.Pending(core.JobApprovalPoll)
	if len(pend) != 1 {
		t.Fatalf("Open queued %d poll jobs", len(pend))
	}
	first := pend[0]
	if !first.ScheduledAt.After(r.fx.Clock.Now()) || first.ScheduledAt.Sub(r.fx.Clock.Now()) > 30*time.Second {
		t.Errorf("first poll at %v (now %v)", first.ScheduledAt, r.fx.Clock.Now())
	}
	// another Open in the same period shares the job
	r.open(t, nil, "m1")
	if n := len(r.fx.Jobs.Pending(core.JobApprovalPoll)); n != 1 {
		t.Errorf("poll jobs = %d", n)
	}
	// the worker runs the handler; the approval is still open, so the next poll is queued
	r.svc.Register(r.fx.Worker)
	r.fx.Clock.Advance(time.Minute)
	if n := r.fx.Worker.Drain(context.Background()); n != 1 {
		t.Fatalf("drained %d jobs", n)
	}
	pend = r.fx.Jobs.Pending(core.JobApprovalPoll)
	if len(pend) != 1 || pend[0].ID == first.ID || !pend[0].ScheduledAt.After(r.fx.Clock.Now()) {
		t.Fatalf("next poll = %+v", pend)
	}
	// once decided, the chain ends
	r.decide(t, core.DecideRequest{Approval: a.ID, All: core.VerdictApprove})
	r.fx.Clock.Advance(time.Minute)
	r.fx.Worker.Drain(context.Background())
	if n := len(r.fx.Jobs.Pending(core.JobApprovalPoll)); n != 0 {
		t.Errorf("poll jobs after everything decided = %d", n)
	}
}

func TestHandlePollPayload(t *testing.T) {
	r := newRig(t)
	if err := r.svc.HandlePoll(context.Background(), core.Job{Tenant: tenant, Payload: []byte(`not json`)}); !core.IsCode(err, core.CodeInternal) {
		t.Errorf("bad payload: %v", err)
	}
	if err := r.svc.HandlePoll(context.Background(), core.Job{Tenant: tenant, Payload: []byte(`{"tenant":"default"}`)}); err != nil {
		t.Errorf("empty poll: %v", err)
	}
}

// tlc poll on a fake exec: the real adapter over scripted `tlc` answers
// decides an approval whose task a person marked done.
func TestPollOverTheTlcAdapter(t *testing.T) {
	fx := coretest.New()
	cmds := &coretest.Commands{}
	cmds.Reply([]string{"tlc", "task", "create"}, core.CommandResult{Stdout: []byte("Created task T-0100: x\n")})
	cmds.Reply([]string{"tlc", "task", "show", "T-0100"}, core.CommandResult{Stdout: []byte(
		`{"task":{"status":"DONE","title":"x"},"logs":[{"id":4,"timestamp":"2026-11-18T12:05:00Z","by":"lead","action":"DONE","note":"Status changed from TODO to DONE: ship it"}]}`)})
	cmds.Reply([]string{"tlc", "task", "complete"}, core.CommandResult{})
	r := newRigWithTasks(t, fx, tlccli.New(cmds, tlccli.Config{}))
	a := r.open(t, func(q *core.ApprovalRequest) {}, "m1")
	if a.TaskRef != "T-0100" {
		t.Fatalf("task ref = %q", a.TaskRef)
	}
	res, err := r.svc.Poll(context.Background(), tenant)
	if err != nil || res.FromTasks != 1 {
		t.Fatalf("poll = %+v %v", res, err)
	}
	got := r.get(t, a.ID)
	if d := got.Items[0].Decision; got.Status != core.ApprovalDecided || d.Verdict != core.VerdictApprove || d.By != "lead" {
		t.Errorf("approval = %+v", got)
	}
	// the task is already done: closing it again ran no tlc complete
	for _, c := range cmds.Calls() {
		if len(c.Argv) > 2 && c.Argv[2] == "complete" {
			t.Errorf("closed a task a person closed: %v", c.Argv)
		}
	}
}
