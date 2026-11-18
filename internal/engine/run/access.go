package run

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
)

// sideEffect is the class the step is admitted under: the node's, or
// write-shared for a core kind (flow.md §5.3).
func (u *unit) sideEffect() node.SideEffect {
	if u.sp.node != nil {
		return u.sp.node.Spec.SideEffect
	}
	return node.SideEffectWriteShared
}

func (u *unit) operation() string {
	if u.sp.node != nil {
		return u.sp.node.Spec.Node
	}
	return string(u.sp.kind)
}

func (u *unit) capabilities() []string {
	if u.sp.node != nil {
		return u.sp.node.Spec.Capabilities
	}
	return nil
}

// access runs the checks in front of a dispatch: the profile holds the
// node's capabilities, an irreversible step has its approval guard, and the
// admission port allows it. A human step runs nothing and is not checked.
// ok is false when res already says how the step ended.
func (u *unit) access(ctx context.Context, in map[string]any) (res stepResult, ok bool) {
	if u.sp.kind == core.StepKindHuman {
		return stepResult{}, true
	}
	if err := u.grants(ctx); err != nil {
		return u.fail(err), false
	}
	if err := u.guard(ctx, in); err != nil {
		return u.fail(err), false
	}
	return u.admit(ctx, in)
}

func (u *unit) grants(ctx context.Context) error {
	var missing []string
	for _, c := range u.capabilities() {
		covered, err := u.t.e.d.Grants.Covers(ctx, u.t.run.Tenant, u.runAs, c)
		if err != nil {
			return err //nolint:wrapcheck // port error passes through
		}
		if !covered {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return core.Errorf(core.CodeGrantDenied, "profile %q lacks %v for step %s", u.runAs, missing, u.sp.step.ID).
		WithDetail("missing", missing)
}

// guard refuses an irreversible step that no approval covers (node.md §10.1).
func (u *unit) guard(ctx context.Context, in map[string]any) error {
	if u.sideEffect() != node.SideEffectIrreversible {
		return nil
	}
	q := core.GuardQuery{Tenant: u.t.run.Tenant, Run: u.t.run.ID, StepID: u.sp.step.ID, Approvals: u.sp.guards}
	if tr := u.sp.node.Spec.Trust; tr != nil {
		class, err := inTemplate(tr.Class, in)
		if err != nil {
			return err
		}
		q.TrustProvider, q.TrustClass = tr.Provider, class
	}
	ok, err := u.t.e.d.ApprovalSvc.GuardSatisfied(ctx, q)
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	if !ok {
		return core.Errorf(core.CodeIrreversibleUnsafe, "step %s is irreversible and no approval covers it", u.sp.step.ID).
			WithSuggestion("put a flo/approval@1 step before it")
	}
	return nil
}

// inRef is a placeholder over the step's inputs.
var inRef = regexp.MustCompile(`^\s*\{\{\s*in\.([A-Za-z0-9_]+(?:\.[A-Za-z0-9_]+)*)\s*\}\}\s*$`)

// inTemplate resolves a node-spec field that may template over the step's
// inputs (trust.class, idempotency_key; node.md §10.1, §11): a literal, or one
// `{{ in.<path> }}` placeholder. `in` is a CEL keyword, so the
// expression environment cannot bind it and the lookup is done here.
func inTemplate(class string, in map[string]any) (string, error) {
	m := inRef.FindStringSubmatch(class)
	if m == nil {
		if strings.Contains(class, "{{") {
			return "", core.Errorf(core.CodeEvalError, "template %q: want a literal or {{ in.<input> }}", class)
		}
		return class, nil
	}
	var cur any = in
	for _, k := range strings.Split(m[1], ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return "", core.Errorf(core.CodeUnresolvedRef, "template %q: no input %s", class, m[1])
		}
		if cur, ok = obj[k]; !ok {
			return "", core.Errorf(core.CodeUnresolvedRef, "template %q: no input %s", class, m[1])
		}
	}
	return fmt.Sprint(cur), nil
}

// admit asks the admission port when the step has a side effect above read
// or acts as a profile other than the starter's (node.md §10.2).
func (u *unit) admit(ctx context.Context, in map[string]any) (stepResult, bool) {
	run := u.t.run
	if u.sideEffect().Rank() <= node.SideEffectRead.Rank() && u.runAs == run.StartedBy {
		return stepResult{}, true
	}
	dec, err := u.t.e.d.Admission.Admit(ctx, core.AdmissionRequest{
		Tenant: run.Tenant, Agent: u.runAs, Principal: run.StartedBy,
		Purpose: core.Purpose{Flow: run.Flow.Name, Version: run.Flow.Version, StepID: u.sp.step.ID},
		Run:     run.ID, StepRun: u.rec.ID, Operation: u.operation(), Capabilities: u.capabilities(),
		SideEffect: u.sideEffect(), Input: in,
	})
	if err != nil {
		return u.fail(err), false
	}
	if !slices.Contains(u.rec.Receipts, dec.Receipt.ID) {
		u.rec.Receipts = append(u.rec.Receipts, dec.Receipt.ID)
	}
	u.t.emitAdmission(ctx, run, u.sp.step.ID, dec)
	switch dec.Result {
	case core.AdmissionAllow:
		return stepResult{}, true
	case core.AdmissionDeferred:
		return u.park(ctx, core.ReasonAdmissionDeferred, waitRef{receipt: dec.Receipt.ID}), false
	default:
		return u.fail(core.Errorf(core.CodeAdmissionDenied, "step %s denied: %s", u.sp.step.ID, dec.Reason).
			WithDetail("receipt", string(dec.Receipt.ID))), false
	}
}
