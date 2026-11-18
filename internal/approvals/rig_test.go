package approvals_test

import (
	"context"
	"testing"

	"hop.top/flo/internal/approvals"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
)

const tenant = core.DefaultTenant

type rig struct {
	fx   *coretest.Fixture
	svc  *approvals.Service
	node *approvals.Node
}

func newRig(t *testing.T) *rig {
	t.Helper()
	fx := coretest.New()
	return newRigWithTasks(t, fx, fx.Tasks)
}

func newRigWithTasks(t *testing.T, fx *coretest.Fixture, tasks core.Tasks) *rig {
	t.Helper()
	svc := approvals.New(approvals.Deps{
		Store: fx.DB.Approvals, Tasks: tasks, Resumer: fx.Resumer, Trust: fx.Trust, Runs: fx.DB.Runs,
		Flows: fx.DB.Flows, Jobs: fx.Jobs, RunLog: fx.RunLog, Clock: fx.Clock, IDs: fx.IDs,
	})
	n, err := approvals.NewNode(approvals.NodeDeps{Approvals: svc, Store: fx.DB.Approvals, Trust: fx.Trust, Clock: fx.Clock})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{fx: fx, svc: svc, node: n}
}

// run creates a run of flow "triage" and returns it.
func (r *rig) run(t *testing.T, id core.RunID) core.Run {
	t.Helper()
	run, _, err := r.fx.DB.Runs.CreateRun(context.Background(), core.NewRun{
		ID: id, Tenant: tenant, Flow: core.FlowRef{Name: "triage", Version: "1", Hash: "h1"},
		Trigger: core.TriggerInfo{Kind: "manual", At: r.fx.Clock.Now()}, StartedBy: "jad", RunAs: "noor",
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func step(id string) core.StepRun {
	return core.StepRun{ID: core.StepRunID("stp_" + id), Tenant: tenant, Run: "run_1", StepID: id, Kind: core.StepKindNode, Node: approvals.NodeType}
}

func items(ids ...string) []core.ApprovalItem {
	out := make([]core.ApprovalItem, len(ids))
	for i, id := range ids {
		out[i] = core.ApprovalItem{ID: id, Action: "archive", Why: "newsletter"}
	}
	return out
}

// open opens an approval of the given items on step "approve" of run_1.
func (r *rig) open(t *testing.T, mut func(*core.ApprovalRequest), ids ...string) core.Approval {
	t.Helper()
	if _, err := r.fx.DB.Runs.GetRun(context.Background(), tenant, "run_1"); err != nil {
		r.run(t, "run_1")
	}
	req := core.ApprovalRequest{Kind: core.ApprovalItems, Title: "Approve triage", Items: items(ids...), Surface: core.ApprovalSurfaceTLC}
	if mut != nil {
		mut(&req)
	}
	a, err := r.svc.Open(context.Background(), tenant, "run_1", step("approve"), req)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (r *rig) decide(t *testing.T, req core.DecideRequest) core.DecideResult {
	t.Helper()
	if req.By == "" {
		req.By = "jad"
	}
	res, err := r.svc.Decide(context.Background(), tenant, req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (r *rig) get(t *testing.T, id core.ApprovalID) core.Approval {
	t.Helper()
	a, err := r.svc.Get(context.Background(), tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (r *rig) topics(run core.RunID) []string {
	var out []string
	for _, tp := range r.fx.RunLog.Topics(tenant, run) {
		out = append(out, string(tp))
	}
	return out
}

func count(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}

func inv(r *rig, in map[string]any) core.Invocation {
	return core.Invocation{
		Tenant: tenant, Run: core.RunContext{ID: "run_1", Flow: "triage", Version: "1", StartedAt: r.fx.Clock.Now()},
		StepRun: "stp_approve", StepID: "approve", Kind: core.StepKindNode, Attempt: 1,
		RunAs: "noor", Principal: "jad", In: in,
	}
}
