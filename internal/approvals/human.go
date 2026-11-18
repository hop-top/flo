package approvals

import (
	"context"
	"time"

	"hop.top/flo/internal/core"
)

// The `decision` of a core `human` step output (flow.md §5.5):
// {decision: approved|rejected, by, at, note}.
const (
	DecisionApproved = "approved"
	DecisionRejected = "rejected"
)

// Human is the executor of the core `human` kind: a whole-step decision,
// the single item HumanItemID of an approval of kind human. Like the
// approval node it has two phases: it returns a park request, and when the
// approval is complete the executor dispatches it again with Resume.
//
// Its timeout takes the step's on_timeout: approve and reject decide the
// item (by the engine, reason timeout); keep_waiting, and no on_timeout,
// wait for ever.
type Human struct{}

// Execute handles one invocation of a human step.
func (Human) Execute(_ context.Context, inv core.Invocation) (core.Result, error) {
	if inv.Resume != nil {
		return humanOutput(inv.Resume.Approval)
	}
	if inv.Human == nil {
		return failed(core.New(core.CodeInputInvalid, "a human step has no human input")), nil
	}
	h := inv.Human
	switch h.OnTimeout {
	case "", OnTimeoutApprove, OnTimeoutReject, OnTimeoutKeepWaiting:
	default:
		return failed(core.Errorf(core.CodeInputInvalid, "human.on_timeout %q: want approve, reject or keep_waiting", h.OnTimeout)), nil
	}
	onTimeout := h.OnTimeout
	if onTimeout == "" {
		onTimeout = OnTimeoutKeepWaiting
	}
	title := h.Title
	if title == "" {
		title = "decide " + inv.StepID
	}
	return core.Result{
		Outcome: core.OutcomePark, Effect: core.EffectNone,
		Park: &core.ApprovalRequest{
			Kind: core.ApprovalHuman, Title: title, Description: h.Description, Surface: core.ApprovalSurfaceTLC,
			Verdicts: humanVerdicts, Items: []core.ApprovalItem{{ID: core.HumanItemID, Action: title}},
			Timeout: h.Timeout, OnTimeout: onTimeout, Assignee: h.Assignee,
		},
	}, nil
}

func humanOutput(a core.Approval) (core.Result, error) {
	it, ok := a.Item(core.HumanItemID)
	if !ok || it.Decision == nil {
		return core.Result{}, core.Errorf(core.CodeInternal, "approval %s has no decision for its human step", a.ID)
	}
	d := it.Decision
	decision := DecisionRejected
	if d.Verdict.Approving() {
		decision = DecisionApproved
	}
	out := map[string]any{"decision": decision, "by": d.By, "at": d.At.UTC().Format(time.RFC3339), "note": d.Reason}
	return core.Result{Outcome: core.OutcomeOK, Output: out, Effect: core.EffectDone}, nil
}
