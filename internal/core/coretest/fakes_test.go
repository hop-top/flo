package coretest_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/kit/go/runtime/bus"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/spec/node"
)

const T = core.DefaultTenant

var bg = context.Background()

func newRun(t *testing.T, f *coretest.Fixture, key string) core.Run {
	t.Helper()
	r, _, err := f.DB.Runs.CreateRun(bg, core.NewRun{ID: core.NewRunID(f.IDs), Tenant: T, Flow: core.FlowRef{Name: "w", Version: "1", Hash: "h"}, IdemKey: key, Group: "w", Admitted: true})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestClockSleepAndAdvance(t *testing.T) {
	c := coretest.NewClock(time.Time{})
	done := make(chan time.Time)
	go func() { _ = c.Sleep(bg, 5*time.Second); done <- c.Now() }()
	c.BlockUntil(1)
	c.Advance(4 * time.Second)
	select {
	case <-done:
		t.Fatal("woke early")
	case <-time.After(20 * time.Millisecond):
	}
	c.Advance(time.Second)
	if got := <-done; !got.Equal(coretest.Epoch.Add(5 * time.Second)) {
		t.Errorf("woke at %v", got)
	}
	ctx, cancel := context.WithCancel(bg)
	go func() { c.BlockUntil(1); cancel() }()
	if err := c.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) || c.Sleepers() != 0 {
		t.Errorf("cancelled sleep: %v, %d sleepers", err, c.Sleepers())
	}
}

func TestIDsAreValidSequentialAndUnique(t *testing.T) {
	ids := &coretest.IDs{}
	seen := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := ids.New(core.PrefixRun)
			mu.Lock()
			defer mu.Unlock()
			if seen[id] {
				t.Errorf("duplicate %s", id)
			}
			seen[id] = true
			if _, err := core.ParseRunID(id); err != nil {
				t.Errorf("%s: %v", id, err)
			}
		}()
	}
	wg.Wait()
	if a, b := (&coretest.IDs{}).New("run"), "run_00000000000000000000000001"; a != b {
		t.Errorf("first id %s, want %s", a, b)
	}
}

func TestRunStoreIdempotencyAndCAS(t *testing.T) {
	f := coretest.New()
	a := newRun(t, f, "k")
	b, created, err := f.DB.Runs.CreateRun(bg, core.NewRun{ID: core.NewRunID(f.IDs), Tenant: T, Flow: a.Flow, IdemKey: "k"})
	if err != nil || created || b.ID != a.ID {
		t.Fatalf("idem create = %+v %v %v", b.ID, created, err)
	}
	if _, err := f.DB.Runs.GetRun(bg, T, "run_nope"); !core.IsCode(err, core.CodeRunNotFound) {
		t.Error("not found")
	}
	if _, err := f.DB.Runs.GetRun(bg, "other", a.ID); !core.IsCode(err, core.CodeRunNotFound) {
		t.Error("tenants are isolated")
	}
	running := core.RunRunning
	if _, err := f.DB.Runs.UpdateRun(bg, T, a.ID, []core.RunStatus{core.RunRunning}, core.RunPatch{Status: &running}); !core.IsCode(err, core.CodeRunStateConflict) {
		t.Error("expect mismatch must conflict")
	}
	got, err := f.DB.Runs.UpdateRun(bg, T, a.ID, []core.RunStatus{core.RunQueued}, core.RunPatch{Status: &running})
	if err != nil || got.Status != core.RunRunning {
		t.Fatalf("update = %+v %v", got.Status, err)
	}
	done := core.RunSucceeded
	if _, err := f.DB.Runs.UpdateRun(bg, T, a.ID, nil, core.RunPatch{Status: &done}); !core.IsCode(err, core.CodeUsage) {
		t.Error("a patch must not set a terminal status")
	}
	got.Vars = map[string]any{"x": 1}
	got2, _ := f.DB.Runs.GetRun(bg, T, a.ID)
	if got2.Vars != nil {
		t.Error("returned records must be copies")
	}
}

func TestStateStagedCommitsOnlyOnSuccess(t *testing.T) {
	f := coretest.New()
	for _, tc := range []struct {
		status core.RunStatus
		want   bool
	}{{core.RunSucceeded, true}, {core.RunFailed, false}, {core.RunCancelled, false}} {
		r := newRun(t, f, "")
		if err := f.DB.State.Stage(bg, T, r.ID, "seen", []any{"a"}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.DB.State.FinishRun(bg, T, r.ID, core.RunFinish{Status: tc.status}); err != nil {
			t.Fatal(err)
		}
		if st, _ := f.DB.State.Staged(bg, T, r.ID); len(st) != 0 {
			t.Errorf("%s: staged writes survive finish", tc.status)
		}
		// finishing again is a no-op; with another status it conflicts
		if _, err := f.DB.State.FinishRun(bg, T, r.ID, core.RunFinish{Status: tc.status}); err != nil {
			t.Errorf("repeat finish: %v", err)
		}
		other := core.RunFailed
		if tc.status == core.RunFailed {
			other = core.RunSucceeded
		}
		if _, err := f.DB.State.FinishRun(bg, T, r.ID, core.RunFinish{Status: other}); !core.IsCode(err, core.CodeRunStateConflict) {
			t.Errorf("conflicting finish: %v", err)
		}
	}
	snap, _ := f.DB.State.Snapshot(bg, T, "w")
	if len(snap) != 1 {
		t.Errorf("only the successful run commits: %v", snap)
	}
}

func TestStateFailAndCancelLeaveStateUntouched(t *testing.T) {
	f := coretest.New()
	ok := newRun(t, f, "")
	_ = f.DB.State.Stage(bg, T, ok.ID, "k", "v1")
	_, _ = f.DB.State.FinishRun(bg, T, ok.ID, core.RunFinish{Status: core.RunSucceeded})
	bad := newRun(t, f, "")
	_ = f.DB.State.Stage(bg, T, bad.ID, "k", "v2")
	_, _ = f.DB.State.FinishRun(bg, T, bad.ID, core.RunFinish{Status: core.RunFailed})
	snap, _ := f.DB.State.Snapshot(bg, T, "w")
	if snap["k"] != "v1" {
		t.Errorf("state = %v", snap)
	}
}

func TestStateTooLarge(t *testing.T) {
	f := coretest.New()
	r := newRun(t, f, "")
	big := strings.Repeat("x", core.MaxStateValueBytes)
	if err := f.DB.State.Stage(bg, T, r.ID, "k", big); !core.IsCode(err, core.CodeStateTooLarge) {
		t.Errorf("err = %v", err)
	}
}

func TestStepRecordsKeepOrderAndNeverRewriteTerminal(t *testing.T) {
	f := coretest.New()
	r := newRun(t, f, "")
	for _, id := range []string{"b", "a"} {
		_ = f.DB.Runs.PutStep(bg, T, core.StepRun{ID: core.StepRunID("s-" + id), Run: r.ID, StepID: id, Status: core.StepRunning})
	}
	_ = f.DB.Runs.PutStep(bg, T, core.StepRun{ID: "s-b", Run: r.ID, StepID: "b", Status: core.StepSucceeded, Output: 1})
	_ = f.DB.Runs.PutStep(bg, T, core.StepRun{ID: "s-b", Run: r.ID, StepID: "b", Status: core.StepFailed})
	steps, _ := f.DB.Runs.ListSteps(bg, T, r.ID)
	if len(steps) != 2 || steps[0].StepID != "b" || steps[0].Status != core.StepSucceeded || steps[1].StepID != "a" {
		t.Errorf("steps = %+v", steps)
	}
}

func TestFlowStoreVersions(t *testing.T) {
	f := coretest.New()
	put := func(hash string) core.FlowVersion {
		v, _, err := f.DB.Flows.PutVersion(bg, core.NewFlowVersion{Tenant: T, Name: "w", Version: "1", Hash: hash, Source: []byte(hash)})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	a := put("h1")
	if _, created, _ := f.DB.Flows.PutVersion(bg, core.NewFlowVersion{Tenant: T, Name: "w", Version: "1", Hash: "h1"}); created {
		t.Error("same bytes must be one version")
	}
	b := put("h2")
	if a.Seq != 1 || b.Seq != 2 {
		t.Errorf("seq %d %d", a.Seq, b.Seq)
	}
	if _, err := f.DB.Flows.Active(bg, T, "w"); !core.IsCode(err, core.CodeFlowNotActive) {
		t.Error("draft is not active")
	}
	_, _ = f.DB.Flows.SetState(bg, T, "w", "h1", core.FlowActive, time.Time{})
	_, _ = f.DB.Flows.SetState(bg, T, "w", "h2", core.FlowActive, time.Time{})
	act, _ := f.DB.Flows.Active(bg, T, "w")
	vs, _ := f.DB.Flows.ListVersions(bg, T, "w")
	if act.Hash != "h2" || vs[0].Hash != "h2" || vs[1].State != core.FlowRetired {
		t.Errorf("active %s, versions %+v", act.Hash, vs)
	}
	if _, err := f.DB.Flows.SetState(bg, T, "w", "h1", core.FlowPaused, time.Time{}); !core.IsCode(err, core.CodeFlowNotActive) {
		t.Error("only an active version pauses")
	}
}

func TestApprovalStoreCAS(t *testing.T) {
	f := coretest.New()
	a, created, err := f.DB.Approvals.CreateApproval(bg, core.Approval{ID: "ap_1", Tenant: T, Run: "r", StepRun: "s", Items: []core.ApprovalItem{{ID: "1"}}})
	if err != nil || !created || a.Version != 1 {
		t.Fatalf("create = %+v %v", a, err)
	}
	if _, created, _ := f.DB.Approvals.CreateApproval(bg, core.Approval{ID: "ap_2", Tenant: T, Run: "r", StepRun: "s"}); created {
		t.Error("idempotent on (run, step run)")
	}
	b, err := f.DB.Approvals.UpdateApproval(bg, a)
	if err != nil || b.Version != 2 {
		t.Fatalf("update = %v %v", b.Version, err)
	}
	if _, err := f.DB.Approvals.UpdateApproval(bg, a); !core.IsCode(err, core.CodeApprovalConflict) {
		t.Errorf("stale update: %v", err)
	}
}

func TestTriggerAdvanceCAS(t *testing.T) {
	f := coretest.New()
	_ = f.DB.Triggers.PutTrigger(bg, core.TriggerRecord{ID: "trg_1", Tenant: T, Flow: "w"})
	at := coretest.Epoch.Add(time.Hour)
	ok, _ := f.DB.Triggers.AdvanceTrigger(bg, T, "trg_1", nil, &at, "job_1")
	again, _ := f.DB.Triggers.AdvanceTrigger(bg, T, "trg_1", nil, &at, "job_2")
	if !ok || again {
		t.Errorf("first %v second %v: only one worker may enqueue the next fire", ok, again)
	}
	moved, _ := f.DB.Triggers.AdvanceTrigger(bg, T, "trg_1", &at, nil, "")
	if !moved {
		t.Error("advance from the stored instant")
	}
}

func TestOutputPrune(t *testing.T) {
	f := coretest.New()
	old := f.Clock.Now().Add(-100 * 24 * time.Hour)
	_ = f.DB.Outputs.PutOutput(bg, core.Output{ID: "out_1", Tenant: T, CreatedAt: old})
	_ = f.DB.Outputs.PutOutput(bg, core.Output{ID: "out_2", Tenant: T})
	n, _ := f.DB.Outputs.PruneOutputs(bg, T, f.Clock.Now().Add(-core.OutputRetention))
	left, _ := f.DB.Outputs.ListOutputs(bg, T, core.OutputFilter{})
	if n != 1 || len(left) != 1 || left[0].ID != "out_2" {
		t.Errorf("pruned %d, left %+v", n, left)
	}
}

func TestJobsDedupeDelayAndWorker(t *testing.T) {
	f := coretest.New()
	var ran []string
	f.Worker.Handle(core.JobRunAdvance, func(_ context.Context, j core.Job) error { ran = append(ran, j.Key); return nil })
	at := f.Clock.Now().Add(time.Minute)
	a, _ := f.Jobs.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "r1", Payload: 1})
	b, _ := f.Jobs.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "r1", Payload: 1})
	_, _ = f.Jobs.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "r2", Payload: 1, At: &at})
	if a != b {
		t.Error("same key must dedupe")
	}
	if n := f.Worker.Drain(bg); n != 1 || len(ran) != 1 || ran[0] != "r1" {
		t.Errorf("drain ran %d %v", n, ran)
	}
	f.Clock.Advance(time.Minute)
	f.Worker.Drain(bg)
	if len(ran) != 2 {
		t.Errorf("delayed job did not run: %v", ran)
	}
	// finished job no longer dedupes
	c, _ := f.Jobs.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "r1"})
	if c == a {
		t.Error("a finished job must not dedupe")
	}
	cancelled, _ := f.Jobs.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "r9"})
	_ = f.Jobs.Cancel(bg, cancelled)
	if info, _ := f.Jobs.Get(bg, cancelled); info.Status != core.JobCancelled {
		t.Error("cancel")
	}
}

func TestWorkerRetriesThenFails(t *testing.T) {
	f := coretest.New()
	calls := 0
	f.Worker.Handle(core.JobRunAdvance, func(context.Context, core.Job) error { calls++; return errors.New("boom") })
	id, _ := f.Jobs.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "r", MaxAttempts: 3})
	f.Worker.Drain(bg)
	if info, _ := f.Jobs.Get(bg, id); calls != 3 || info.Status != core.JobFailed {
		t.Errorf("calls %d status %s", calls, info.Status)
	}
}

func TestRunLogOrderFollowAndValidation(t *testing.T) {
	f := coretest.New()
	emit := func(topic string) {
		ev := core.NewEvent(f.Clock, f.IDs, bus.Topic("flo.run.run."+topic), T, "run_1", core.RunEvent{Tenant: T, Run: "run_1", Status: core.RunRunning})
		if err := f.RunLog.Emit(bg, T, "run_1", ev); err != nil {
			t.Fatal(err)
		}
	}
	emit("started")
	r, _ := f.RunLog.Open(bg, T, "run_1", true)
	got := make(chan string, 4)
	go func() {
		for {
			ev, err := r.Next(bg)
			if err != nil {
				got <- err.Error()
				return
			}
			got <- string(ev.Topic)
		}
	}()
	if v := <-got; v != "flo.run.run.started" {
		t.Errorf("first = %s", v)
	}
	emit("succeeded")
	if v := <-got; v != "flo.run.run.succeeded" {
		t.Errorf("second = %s", v)
	}
	if v := <-got; v != io.EOF.Error() {
		t.Errorf("follow must end after the terminal event, got %s", v)
	}
	evs := f.RunLog.Events(T, "run_1")
	if evs[0].Extensions[core.ExtSeq] != int64(1) || evs[1].Extensions[core.ExtSeq] != int64(2) {
		t.Errorf("seq = %v %v", evs[0].Extensions, evs[1].Extensions)
	}
	bad := core.NewEvent(f.Clock, f.IDs, "not.a.topic", T, "run_1", nil)
	if err := f.RunLog.Emit(bg, T, "run_1", bad); err == nil {
		t.Error("an invalid topic must be refused")
	}
}

func spec(typ string, se node.SideEffect) *node.Spec {
	return &node.Spec{Node: typ, Kind: node.KindCommand, SideEffect: se, Privacy: node.PrivacyLocal}
}

func TestNodeRegistryPrecedence(t *testing.T) {
	var n coretest.Nodes
	n.AddBuiltin(spec("x/y@2", node.SideEffectNone))
	n.Add(spec("x/y@1", node.SideEffectNone), core.NodeSource{Layer: core.LayerBundle})
	n.Add(spec("x/z@1", node.SideEffectNone), core.NodeSource{Layer: core.LayerPack, Pack: "p1"})
	n.Add(spec("x/z@1", node.SideEffectNone), core.NodeSource{Layer: core.LayerPack, Pack: "p2"})
	got, err := n.Resolve(bg, core.NodeScope{}, "x/y")
	if err != nil || got.Spec.Node != "x/y@1" {
		t.Errorf("bundle must win over a higher built-in major: %v %v", got.Spec, err)
	}
	if got, _ := n.Resolve(bg, core.NodeScope{}, "x/y@2"); got.Source.Layer != core.LayerBuiltin {
		t.Error("a pinned major selects it")
	}
	if _, err := n.Resolve(bg, core.NodeScope{}, "x/z@1"); !core.IsCode(err, core.CodeNodeConflict) {
		t.Errorf("duplicate across packs: %v", err)
	}
	if _, err := n.Resolve(bg, core.NodeScope{}, "x/none@1"); !core.IsCode(err, core.CodeNodeNotFound) {
		t.Error("not found")
	}
}

func TestScriptRecordsAndTracksConcurrency(t *testing.T) {
	var r coretest.Runner
	release := make(chan struct{})
	r.Handle(func(core.Invocation) bool { return true }, func(core.Invocation) (core.Result, error) {
		<-release
		return coretest.OK(1), nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = r.Run(bg, core.Invocation{StepID: "s"}) }()
	}
	for r.MaxInFlight() < 4 {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	if len(r.CallsFor("s")) != 4 || r.MaxInFlight() != 4 {
		t.Errorf("calls %d max %d", len(r.CallsFor("s")), r.MaxInFlight())
	}
	var s coretest.Script
	s.ForStep("a", coretest.Fail(core.CodeTimeout, "t", core.EffectUnknown), coretest.OK("ok"))
	first, _ := s.Dispatcher().Dispatch(bg, core.Invocation{StepID: "a"})
	second, _ := s.Dispatcher().Dispatch(bg, core.Invocation{StepID: "a"})
	third, _ := s.Dispatcher().Dispatch(bg, core.Invocation{StepID: "a"})
	if first.Outcome != core.OutcomeFailed || second.Output != "ok" || third.Output != "ok" {
		t.Error("ForStep sequence: the last result repeats")
	}
}

func TestCommandsMatchByPrefix(t *testing.T) {
	var c coretest.Commands
	c.Reply([]string{"gh", "api"}, core.CommandResult{ExitCode: 0, Stdout: []byte("{}")})
	res, _ := c.Run(bg, core.CommandSpec{Argv: []string{"gh", "api", "x"}, Profile: "noor"})
	miss, _ := c.Run(bg, core.CommandSpec{Argv: []string{"ls"}})
	if string(res.Stdout) != "{}" || miss.ExitCode != 127 || len(c.Calls()) != 2 || c.Calls()[0].Profile != "noor" {
		t.Errorf("res %+v miss %+v", res, miss)
	}
}

func TestGrantsDenyByDefault(t *testing.T) {
	f := coretest.New()
	g := f.Grants
	if ok, _ := g.Covers(bg, T, "noor", "mail.send"); ok {
		t.Error("deny by default")
	}
	_, _ = g.Set(bg, T, "owner", core.CapabilityAny)
	if ok, _ := g.Covers(bg, T, "owner", "anything.x"); !ok {
		t.Error("wildcard")
	}
	changed, _ := g.Set(bg, T, "owner", core.CapabilityAny)
	if changed {
		t.Error("set is idempotent")
	}
	_, _ = g.Set(bg, T, "noor", "vcs.read")
	_, _ = g.Set(bg, T, "owner", coretest.ProfileUsePrefix+"noor")
	err := g.CheckFlow(bg, core.GrantCheck{Tenant: T, Caller: "owner", RunAs: "noor", Capabilities: []string{"vcs.read", "mail.send"}})
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Code != core.CodeGrantDenied || ce.Detail["missing"].([]string)[0] != "mail.send" {
		t.Errorf("CheckFlow = %v", err)
	}
	_ = g.Provision(bg, T, "noor", "tasks.write")
	if _, err := g.Unset(bg, T, "noor", "tasks.write"); !core.IsCode(err, core.CodeGrantProvisioned) {
		t.Errorf("provisioned unset: %v", err)
	}
}

func TestAdmissionIdempotentAndDeferred(t *testing.T) {
	f := coretest.New()
	f.Admission.Policy = func(core.AdmissionRequest) (core.AdmissionResult, string) { return core.AdmissionDeferred, "ask bos" }
	req := core.AdmissionRequest{Tenant: T, Agent: "noor", Run: "r", StepRun: "s", Operation: "x/y@1", Input: map[string]any{"a": 1}}
	d1, _ := f.Admission.Admit(bg, req)
	d2, _ := f.Admission.Admit(bg, req)
	if d1.Result != core.AdmissionDeferred || d1.Receipt.ID != d2.Receipt.ID || d1.Receipt.Digest == "" {
		t.Fatalf("d1 %+v d2 %+v", d1, d2)
	}
	_ = f.Admission.Resolve(bg, T, d1.Receipt.ID, core.AdmissionAllow, "")
	d3, _ := f.Admission.Admit(bg, req)
	if d3.Result != core.AdmissionAllow {
		t.Error("a resolved receipt answers the repeat")
	}
}

func openApproval(t *testing.T, f *coretest.Fixture, items ...core.ApprovalItem) core.Approval {
	t.Helper()
	a, err := f.Approvals.Open(bg, T, "run_1", core.StepRun{ID: "stp_1", StepID: "review"}, core.ApprovalRequest{Title: "triage", Items: items})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestApprovalServiceDecideIsIdempotentAndResumesOnce(t *testing.T) {
	f := coretest.New()
	a := openApproval(t, f, core.ApprovalItem{ID: "1"}, core.ApprovalItem{ID: "2"})
	if a.TaskRef == "" || a.Status != core.ApprovalOpen || len(f.Tasks.Specs()) != 1 {
		t.Fatalf("open = %+v", a)
	}
	r1, err := f.Approvals.Decide(bg, T, core.DecideRequest{Approval: a.ID, By: "owner", Items: []core.ItemVerdict{{Item: "1", Verdict: core.VerdictApprove}}})
	if err != nil || !r1.Changed || r1.Resumed {
		t.Fatalf("r1 = %+v %v", r1, err)
	}
	rep, _ := f.Approvals.Decide(bg, T, core.DecideRequest{Approval: a.ID, By: "owner", Items: []core.ItemVerdict{{Item: "1", Verdict: core.VerdictReject}}})
	if rep.Changed {
		t.Error("an item keeps its first decision")
	}
	r2, _ := f.Approvals.Decide(bg, T, core.DecideRequest{Approval: a.ID, By: "owner", All: core.VerdictReject, AllReason: "no"})
	again, _ := f.Approvals.Decide(bg, T, core.DecideRequest{Approval: a.ID, By: "owner", All: core.VerdictApprove})
	if !r2.Resumed || again.Changed || len(f.Resumer.Calls()) != 1 {
		t.Errorf("r2 %+v again %+v resumes %v", r2.Resumed, again.Changed, f.Resumer.Calls())
	}
	if ti, _ := f.Tasks.Get(bg, T, core.TaskRef(a.TaskRef)); ti.State != core.TaskApproved {
		t.Errorf("task %s", ti.State)
	}
	if _, err := f.Approvals.Decide(bg, T, core.DecideRequest{Approval: a.ID, Items: []core.ItemVerdict{{Item: "9", Verdict: core.VerdictApprove}}}); !core.IsCode(err, core.CodeItemNotFound) {
		t.Errorf("unknown item: %v", err)
	}
}

func TestApprovalServiceAllAutoCompletesAtOpenAndGuard(t *testing.T) {
	f := coretest.New()
	auto := core.ApprovalItem{ID: "1", Decision: &core.ItemDecision{Verdict: core.VerdictAuto, By: core.DeciderTrust}}
	a := openApproval(t, f, auto)
	if a.Status != core.ApprovalDecided || a.TaskRef != "" {
		t.Errorf("all-auto: %+v", a)
	}
	q := core.GuardQuery{Tenant: T, Run: "run_1", StepID: "send", Approvals: []string{"review"}}
	if ok, _ := f.Approvals.GuardSatisfied(bg, q); !ok {
		t.Error("a decided guarding approval satisfies the guard")
	}
	q.Approvals = []string{"other"}
	if ok, _ := f.Approvals.GuardSatisfied(bg, q); ok {
		t.Error("an unrelated approval must not")
	}
	f.Trust.SetLevels("inbox", core.TrustLevel{Class: "archive", Level: core.TrustAuto})
	q.TrustProvider, q.TrustClass = "inbox", "archive"
	if ok, _ := f.Approvals.GuardSatisfied(bg, q); !ok {
		t.Error("trust at auto guards")
	}
}

func TestApprovalServiceCancelRun(t *testing.T) {
	f := coretest.New()
	a := openApproval(t, f, core.ApprovalItem{ID: "1"})
	if err := f.Approvals.CancelRun(bg, T, "run_1"); err != nil {
		t.Fatal(err)
	}
	got, _ := f.Approvals.Get(bg, T, a.ID)
	d := got.Items[0].Decision
	if got.Status != core.ApprovalCancelled || d.Verdict != core.VerdictReject || d.Reason != core.DecisionReasonRunCancelled || d.By != core.DeciderEngine {
		t.Errorf("cancelled = %+v", got)
	}
	if _, err := f.Approvals.Decide(bg, T, core.DecideRequest{Approval: a.ID, All: core.VerdictApprove}); !core.IsCode(err, core.CodeApprovalAlreadyClosed) {
		t.Errorf("decide after cancel: %v", err)
	}
	if len(f.Trust.DecideCalls()) != 0 {
		t.Error("cancel rejections are never reported to trust")
	}
}

// The executor runs steps in goroutines: hammer the stores under -race.
func TestConcurrentUse(t *testing.T) {
	f := coretest.New()
	r := newRun(t, f, "")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := core.NewStepRunID(f.IDs)
			_ = f.DB.Runs.PutStep(bg, T, core.StepRun{ID: id, Run: r.ID, StepID: "s", Status: core.StepRunning})
			_ = f.DB.State.Stage(bg, T, r.ID, "k", i)
			_, _ = f.DB.Runs.ListSteps(bg, T, r.ID)
			_, _ = f.Jobs.Enqueue(bg, core.JobSpec{Type: core.JobRunAdvance, Tenant: T, Key: "k"})
			_ = f.RunLog.Emit(bg, T, r.ID, core.NewEvent(f.Clock, f.IDs, core.TopicStepStarted, T, r.ID, core.StepEvent{Tenant: T, Run: r.ID}))
		}()
	}
	wg.Wait()
	steps, _ := f.DB.Runs.ListSteps(bg, T, r.ID)
	if len(steps) != 20 || len(f.RunLog.Events(T, r.ID)) != 20 || len(f.Jobs.Pending(core.JobRunAdvance)) != 1 {
		t.Errorf("steps %d events %d", len(steps), len(f.RunLog.Events(T, r.ID)))
	}
}
