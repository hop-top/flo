package approvals

import (
	"context"
	_ "embed" // the node spec is shipped with the engine
	"fmt"
	"sync"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec"
	"hop.top/flo/spec/node"
)

// NodeType is the type of the built-in approval node.
const NodeType = "flo/approval@1"

//go:embed approval.node.yaml
var nodeSpecYAML []byte

var (
	specOnce sync.Once
	nodeSpec *node.Spec
	specErr  error
)

// Spec returns the spec of flo/approval@1, shipped with the engine.
func Spec() (*node.Spec, error) {
	specOnce.Do(func() { nodeSpec, specErr = node.Decode(nodeSpecYAML, "approval.node.yaml") })
	return nodeSpec, specErr //nolint:wrapcheck // decode errors name the document
}

// NodeDeps is what the node works with.
type NodeDeps struct {
	// Approvals records the decision of an approval that needs no person.
	Approvals core.ApprovalService
	// Store marks an approval as reported to its trust provider.
	Store core.ApprovalStore
	Trust core.Trust
	Clock core.Clock
}

// Node is the built-in flo/approval@1 (node.md §6.6), a two-phase node.
//
// Phase 1 reads the trust levels, decides the items trust approves (verdict
// auto) and, when every item is auto, succeeds at once, recording the
// decision; otherwise it returns a park request and the executor opens the
// approval. When the approval is complete the executor dispatches the node
// again with Invocation.Resume: phase 2 reports the person's decisions to
// the trust provider, once, and turns the decisions into the output.
type Node struct {
	deps NodeDeps
	spec *node.Spec
}

var _ core.BuiltinNode = (*Node)(nil)

// NewNode returns the approval node.
func NewNode(d NodeDeps) (*Node, error) {
	sp, err := Spec()
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "flo/approval@1 spec: %w", err)
	}
	return &Node{deps: d, spec: sp}, nil
}

// Spec implements core.BuiltinNode.
func (n *Node) Spec() *node.Spec { return n.spec }

// Execute implements core.BuiltinNode.
func (n *Node) Execute(ctx context.Context, inv core.Invocation) (core.Result, error) {
	if inv.Resume != nil {
		return n.finish(ctx, inv.Tenant, inv.Resume.Approval)
	}
	req, err := n.request(ctx, inv)
	if err != nil {
		return failed(err), nil
	}
	if !allAuto(req.Items) {
		return core.Result{Outcome: core.OutcomePark, Park: &req, Effect: core.EffectNone}, nil
	}
	// nothing for a person: record the decision, so the guard of the
	// irreversible step has an approval to find, and succeed
	a, err := n.deps.Approvals.Open(ctx, inv.Tenant, inv.Run.ID, core.StepRun{
		ID: inv.StepRun, Tenant: inv.Tenant, Run: inv.Run.ID, StepID: inv.StepID, Kind: inv.Kind,
		Node: NodeType,
	}, req)
	if err != nil {
		return core.Result{}, err //nolint:wrapcheck // engine fault
	}
	return core.Result{Outcome: core.OutcomeOK, Output: output(a), Effect: core.EffectDone}, nil
}

func failed(err error) core.Result {
	f := core.FailureOf(err)
	return core.Result{Outcome: core.OutcomeFailed, Err: f.Err(), Effect: core.EffectNone}
}

func allAuto(items []core.ApprovalItem) bool {
	for _, it := range items {
		if it.Decision == nil || it.Decision.Verdict != core.VerdictAuto {
			return false
		}
	}
	return true
}

// request reads the inputs and builds the approval request, with trust's
// verdict on the items it approves.
func (n *Node) request(ctx context.Context, inv core.Invocation) (core.ApprovalRequest, error) {
	req := core.ApprovalRequest{Kind: core.ApprovalItems, Surface: core.ApprovalSurfaceTLC, OnTimeout: OnTimeoutReject}
	items, err := parseItems(inv.In["items"])
	if err != nil {
		return req, err
	}
	if s, ok := inv.In["surface"].(string); ok && s != "" {
		if s != core.ApprovalSurfaceTLC && s != core.ApprovalSurfaceNone {
			return req, core.Errorf(core.CodeInputInvalid, "surface %q: want tlc or none", s)
		}
		req.Surface = s
	}
	req.Trust, _ = inv.In["trust"].(string)
	req.Context, _ = inv.In["context"].(map[string]any)
	req.Title, _ = inv.In["title"].(string)
	if req.Title == "" {
		req.Title = fmt.Sprintf("approve %s (%s)", inv.StepID, inv.Run.Flow)
	}
	if v, ok := inv.In["on_timeout"].(string); ok && v != "" && v != OnTimeoutReject {
		return req, core.Errorf(core.CodeInputInvalid, "on_timeout %q: reject is the only value", v)
	}
	if t, ok := inv.In["timeout"].(string); ok && t != "" {
		if req.Timeout, err = spec.ParseDuration(t); err != nil {
			return req, core.Errorf(core.CodeInputInvalid, "timeout: %w", err)
		}
	}
	if req.Verdicts, err = parseVerdicts(inv.In["verdicts"]); err != nil {
		return req, err
	}
	for i := range items {
		if err := itemDigest(inv.Tenant, inv.RunAs, inv.Principal, &items[i]); err != nil {
			return req, err
		}
	}
	if req.Trust != "" {
		if err := n.autoItems(ctx, inv.Tenant, inv.RunAs, req, items); err != nil {
			return req, err
		}
	}
	req.Items = items
	return req, nil
}

// autoItems gives the verdict auto to every item whose classes trust has all
// at level auto. An item without a class is never auto.
func (n *Node) autoItems(ctx context.Context, tenant core.TenantID, runAs string, req core.ApprovalRequest, items []core.ApprovalItem) error {
	if n.deps.Trust == nil {
		return core.Errorf(core.CodeInternal, "trust provider %q is named and the engine has no trust port", req.Trust)
	}
	var classes []string
	for _, it := range items {
		classes = append(classes, it.Class...)
	}
	levels, err := n.deps.Trust.Levels(ctx, tenant, req.Trust, TrustContext(req.Context, runAs, classes))
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	auto := map[string]bool{}
	for _, l := range levels {
		if l.Level == core.TrustAuto {
			auto[l.Class] = true
		}
	}
	now := n.deps.Clock.Now()
	for i, it := range items {
		if len(it.Class) == 0 {
			continue
		}
		all := true
		for _, c := range it.Class {
			all = all && auto[c]
		}
		if all {
			items[i].Decision = &core.ItemDecision{Verdict: core.VerdictAuto, By: core.DeciderTrust, At: now}
		}
	}
	return nil
}

func parseVerdicts(v any) ([]core.Verdict, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, core.Errorf(core.CodeInputInvalid, "verdicts: want a list, got %T", v)
	}
	out := make([]core.Verdict, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		verdict := core.Verdict(s)
		if verdict == core.VerdictAuto || !verdict.Valid() {
			return nil, core.Errorf(core.CodeInputInvalid, "verdicts: %v is not approve, reject or correct", x)
		}
		out = append(out, verdict)
	}
	return out, nil
}

// finish is phase 2: report the person's decisions to the provider, once,
// then build the output.
func (n *Node) finish(ctx context.Context, tenant core.TenantID, a core.Approval) (core.Result, error) {
	if a.Status == core.ApprovalOpen || !a.Complete() {
		return core.Result{}, core.Errorf(core.CodeInternal, "approval %s was dispatched again before it was complete", a.ID)
	}
	if a.Trust != "" && !a.Reported && a.Status != core.ApprovalCancelled {
		res, err := n.report(ctx, tenant, a)
		if err != nil {
			return core.Result{}, err
		}
		if res != nil {
			return *res, nil
		}
	}
	return core.Result{Outcome: core.OutcomeOK, Output: output(a), Effect: core.EffectDone}, nil
}

// report sends the decisions a person made to the trust provider and marks
// the approval reported. Items trust approved (auto) and the engine's own
// rejections (a timeout) are not a person's decisions and are not sent; the
// rejections of a cancelled run never get here. A provider that skips an
// entry fails the step (decision_not_recorded).
func (n *Node) report(ctx context.Context, tenant core.TenantID, a core.Approval) (*core.Result, error) {
	var ds []core.TrustDecision
	for _, it := range a.Items {
		d := it.Decision
		if d == nil || d.By == core.DeciderEngine || d.By == core.DeciderTrust || d.Verdict == core.VerdictAuto {
			continue
		}
		ds = append(ds, core.TrustDecision{ID: it.ID, Verdict: d.Verdict, Reason: d.Reason, Item: itemValue(it)})
	}
	if len(ds) > 0 {
		applied, err := n.deps.Trust.Decide(ctx, tenant, a.Trust, a.Context, ds)
		if err != nil {
			f := failed(err)
			return &f, nil
		}
		if len(applied.Skipped) > 0 {
			f := failed(core.Errorf(core.CodeDecisionNotRecorded, "trust provider %s skipped %v", a.Trust, applied.Skipped).
				WithDetail("skipped", applied.Skipped).WithDetail("applied", applied.Applied))
			// the decisions were reported: not again on a retry
			if err := n.markReported(ctx, a); err != nil {
				return nil, err
			}
			return &f, nil
		}
	}
	return nil, n.markReported(ctx, a)
}

func (n *Node) markReported(ctx context.Context, a core.Approval) error {
	for attempt := 0; ; attempt++ {
		a.Reported = true
		_, err := n.deps.Store.UpdateApproval(ctx, a)
		if core.IsCode(err, core.CodeApprovalConflict) && attempt < 3 {
			if a, err = n.deps.Store.GetApproval(ctx, a.Tenant, a.ID); err != nil {
				return err //nolint:wrapcheck // port error passes through
			}
			if a.Reported {
				return nil
			}
			continue
		}
		return err //nolint:wrapcheck // port error passes through
	}
}
