package run

import (
	"context"

	"hop.top/kit/go/runtime/bus"

	"hop.top/flo/internal/core"
)

// Emitting never fails the caller's work (RunLog.Emit): errors are dropped.

func (t *turn) emitRun(ctx context.Context, topic bus.Topic, f *core.Failure) {
	r := t.run
	u := t.meter.Usage()
	ev := core.NewEvent(t.e.d.Clock, t.e.d.IDs, topic, r.Tenant, r.ID, core.RunEvent{
		Tenant: r.Tenant, Run: r.ID, Flow: r.Flow.Name, Version: r.Flow.Version, Hash: r.Flow.Hash,
		Status: r.Status, Trigger: r.Trigger.Kind, WaitingFor: r.WaitingFor, Failure: f, Usage: &u,
	})
	_ = t.e.d.RunLog.Emit(ctx, r.Tenant, r.ID, ev)
}

// stepTopics maps a step status to the topic that reports reaching it.
var stepTopics = map[core.StepStatus]bus.Topic{
	core.StepRunning:   core.TopicStepStarted,
	core.StepWaiting:   core.TopicStepParked,
	core.StepSucceeded: core.TopicStepSucceeded,
	core.StepFailed:    core.TopicStepFailed,
	core.StepSkipped:   core.TopicStepSkipped,
	core.StepCancelled: core.TopicStepCancelled,
}

// emitStep reports the record's current status. attempt is the 1-based
// attempt number, 0 when the event is not about one.
func (t *turn) emitStep(ctx context.Context, rec core.StepRun, attempt int) {
	t.emitStepAs(ctx, stepTopics[rec.Status], rec, attempt)
}

func (t *turn) emitStepAs(ctx context.Context, topic bus.Topic, rec core.StepRun, attempt int) {
	if topic == "" {
		return
	}
	var idx *int
	if rec.Element != nil {
		idx = &rec.Element.Index
	}
	ev := core.NewEvent(t.e.d.Clock, t.e.d.IDs, topic, rec.Tenant, rec.Run, core.StepEvent{
		Tenant: rec.Tenant, Run: rec.Run, StepRun: rec.ID, Step: rec.StepID, Index: idx, Node: rec.Node,
		Status: rec.Status, Reason: rec.Reason, Attempt: attempt, Inputs: rec.Inputs, Output: rec.Output,
		Cost: rec.Cost, Failure: rec.Failure, Receipts: rec.Receipts,
	})
	_ = t.e.d.RunLog.Emit(ctx, rec.Tenant, rec.Run, ev)
}

func (t *turn) emitAdmission(ctx context.Context, run core.Run, step string, dec core.AdmissionDecision) {
	r := dec.Receipt
	ev := core.NewEvent(t.e.d.Clock, t.e.d.IDs, core.TopicAdmissionDecided, run.Tenant, run.ID, core.AdmissionEvent{
		Tenant: run.Tenant, Run: run.ID, Step: step, Receipt: r.ID, Digest: r.Digest, Decision: dec.Result,
		Agent: r.Agent, Principal: r.Principal, Operation: r.Operation, Reason: dec.Reason,
	})
	_ = t.e.d.RunLog.Emit(ctx, run.Tenant, run.ID, ev)
}
