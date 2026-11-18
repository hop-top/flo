package builtin

import (
	"context"

	"hop.top/flo/internal/core"
)

// newOutput builds flo/output@1 (node.md §6.7): it writes the result to the
// output store, which `flo brief` lists.
func newOutput(d Deps) (core.BuiltinNode, error) {
	s, err := Spec("flo/output@1")
	if err != nil {
		return nil, err
	}
	if d.Outputs == nil || d.IDs == nil || d.Clock == nil {
		return nil, core.Errorf(core.CodeInternal, "flo/output@1 needs an output store, ids and a clock")
	}
	return fn{s, func(ctx context.Context, inv core.Invocation) (core.Result, error) { return publish(ctx, d, inv) }}, nil
}

func publish(ctx context.Context, d Deps, inv core.Invocation) (core.Result, error) {
	kind := core.OutputKind(asString(inv.In["kind"]))
	out := core.Output{
		ID: core.NewOutputID(d.IDs), Tenant: inv.Tenant, Run: inv.Run.ID, StepID: inv.StepID,
		Kind: kind, Title: asString(inv.In["title"]), Body: asString(inv.In["body"]), CreatedAt: d.Clock.Now(),
	}
	switch kind {
	case core.OutputBrief:
		if out.Body == "" {
			return pureFail(core.Errorf(core.CodeInputInvalid, "a brief needs a body")), nil
		}
	case core.OutputFindings:
		items, ok := list(inv.In["items"])
		if !ok || inv.In["items"] == nil {
			return pureFail(core.Errorf(core.CodeInputInvalid, "findings need items")), nil
		}
		out.Items = items
	default:
		return pureFail(core.Errorf(core.CodeInputInvalid, "kind %q is not findings or brief", kind)), nil
	}
	if err := d.Outputs.PutOutput(ctx, out); err != nil {
		return core.Result{}, err //nolint:wrapcheck // the store's own classified error
	}
	return core.Result{
		Outcome: core.OutcomeOK, Output: map[string]any{"id": string(out.ID), "kind": string(kind)}, Effect: core.EffectDone,
	}, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
