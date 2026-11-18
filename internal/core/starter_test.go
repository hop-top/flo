package core_test

import (
	"context"
	"fmt"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	specrun "hop.top/flo/spec/run"
)

const demoFlow = `flow: demo
version: 1.0.0
on:
  - schedule: { rrule: "FREQ=MINUTELY;INTERVAL=15" }
  - manual: {}
%s
steps:
  - id: a
    kind: exec
    exec: { argv: [echo, hi] }
`

func setup(t *testing.T, extra string) (*coretest.Fixture, context.Context) {
	t.Helper()
	f := coretest.New()
	ctx := context.Background()
	src := []byte(fmt.Sprintf(demoFlow, extra))
	v, _, err := f.DB.Flows.PutVersion(ctx, core.NewFlowVersion{Tenant: core.DefaultTenant, Name: "demo", Version: "1.0.0", Hash: "h1", Source: src})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.DB.Flows.SetState(ctx, core.DefaultTenant, "demo", v.Hash, core.FlowActive, f.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	return f, ctx
}

func req(kind specrun.TriggerKind) core.StartRequest {
	return core.StartRequest{Tenant: core.DefaultTenant, Flow: "demo", StartedBy: "owner", Trigger: core.TriggerInfo{Kind: kind}}
}

func TestStartCreatesRunAndAdvanceJob(t *testing.T) {
	f, ctx := setup(t, "")
	res, err := f.Starter.Start(ctx, req(specrun.TriggerManual))
	if err != nil || !res.Created || res.Queued || res.Skipped {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if res.Run.Status != core.RunQueued || !res.Run.Admitted || res.Run.Group != "demo" || res.Run.RunAs != "owner" || res.Run.Flow.Hash != "h1" {
		t.Errorf("run = %+v", res.Run)
	}
	jobs := f.Jobs.Pending(core.JobRunAdvance)
	if len(jobs) != 1 || jobs[0].Key != string(res.Run.ID) {
		t.Errorf("jobs = %+v", jobs)
	}
	if topics := f.RunLog.Topics(core.DefaultTenant, res.Run.ID); len(topics) != 1 || topics[0] != core.TopicRunCreated {
		t.Errorf("events = %v", topics)
	}
}

func TestStartIdempotencyKey(t *testing.T) {
	f, ctx := setup(t, "")
	r := req(specrun.TriggerManual)
	r.IdemKey = "k1"
	a, _ := f.Starter.Start(ctx, r)
	b, err := f.Starter.Start(ctx, r)
	if err != nil || b.Created || b.Run.ID != a.Run.ID {
		t.Fatalf("second start = %+v, %v", b, err)
	}
	if n := len(f.Jobs.Pending(core.JobRunAdvance)); n != 1 {
		t.Errorf("%d advance jobs, want 1", n)
	}
}

func TestStartOverlapPolicies(t *testing.T) {
	// schedule default is skip, manual default is queue
	f, ctx := setup(t, "")
	first, _ := f.Starter.Start(ctx, req(specrun.TriggerManual))
	sk, err := f.Starter.Start(ctx, req(specrun.TriggerSchedule))
	if err != nil || !sk.Skipped || sk.Created {
		t.Fatalf("schedule overlap = %+v, %v", sk, err)
	}
	q, err := f.Starter.Start(ctx, req(specrun.TriggerManual))
	if err != nil || !q.Created || !q.Queued || q.Run.Admitted {
		t.Fatalf("manual overlap = %+v, %v", q, err)
	}
	// finishing the first promotes the queued run
	if _, err := f.DB.State.FinishRun(ctx, core.DefaultTenant, first.Run.ID, core.RunFinish{Status: core.RunSucceeded}); err != nil {
		t.Fatal(err)
	}
	if err := f.Starter.Release(ctx, first.Run); err != nil {
		t.Fatal(err)
	}
	got, _ := f.DB.Runs.GetRun(ctx, core.DefaultTenant, q.Run.ID)
	if !got.Admitted {
		t.Error("queued run was not promoted")
	}
	found := false
	for _, j := range f.Jobs.Pending(core.JobRunAdvance) {
		found = found || j.Key == string(q.Run.ID)
	}
	if !found {
		t.Error("promoted run has no advance job")
	}
}

func TestStartCancelPrevious(t *testing.T) {
	f, ctx := setup(t, "concurrency: { on_overlap: cancel_previous }")
	first, _ := f.Starter.Start(ctx, req(specrun.TriggerManual))
	second, err := f.Starter.Start(ctx, req(specrun.TriggerManual))
	if err != nil || !second.Created || len(second.Cancelled) != 1 || second.Cancelled[0] != first.Run.ID {
		t.Fatalf("second = %+v, %v", second, err)
	}
	got, _ := f.DB.Runs.GetRun(ctx, core.DefaultTenant, first.Run.ID)
	if !got.CancelRequested {
		t.Error("the previous run was not asked to cancel")
	}
}

func TestStartGroupTemplate(t *testing.T) {
	f, ctx := setup(t, `concurrency: { group: "g-{{ trigger.event.uid }}" }`)
	r := req(specrun.TriggerManual)
	r.Trigger.Payload = map[string]any{"event": map[string]any{"uid": "u1"}}
	res, err := f.Starter.Start(ctx, r)
	if err != nil || res.Run.Group != "g-u1" {
		t.Fatalf("res = %+v, %v", res.Run.Group, err)
	}
}

func TestStartErrors(t *testing.T) {
	f, ctx := setup(t, "")
	r := req(specrun.TriggerManual)
	r.Flow = "ghost"
	if _, err := f.Starter.Start(ctx, r); !core.IsCode(err, core.CodeFlowNotFound) {
		t.Errorf("unknown flow: %v", err)
	}
	if _, err := f.DB.Flows.SetState(ctx, core.DefaultTenant, "demo", "h1", core.FlowPaused, f.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Starter.Start(ctx, req(specrun.TriggerManual)); !core.IsCode(err, core.CodeFlowNotActive) {
		t.Errorf("paused flow: %v", err)
	}
	r = req(specrun.TriggerManual)
	r.Version = "h1"
	if res, err := f.Starter.Start(ctx, r); err != nil || !res.Created {
		t.Errorf("a pinned version starts even when not active: %+v %v", res, err)
	}
	r.Tenant = "BAD"
	if _, err := f.Starter.Start(ctx, r); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("bad tenant: %v", err)
	}
}
