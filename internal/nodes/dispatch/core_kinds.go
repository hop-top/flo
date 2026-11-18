package dispatch

import (
	"context"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/command"
)

// exec runs a core exec step: `aps run <run_as> -- argv`, the result the exec
// result object (flow.md §5.3). Its side effect is unknown, so under dry run
// it is treated as write-shared and returns its plan.
func (x *Dispatcher) exec(ctx context.Context, inv core.Invocation) (core.Result, error) {
	ex := inv.Exec
	if ex == nil {
		return core.Result{}, core.Errorf(core.CodeInternal, "an exec step carries its rendered exec")
	}
	env := map[string]string{}
	for k, v := range ex.Env {
		env[k] = v
	}
	if inv.IdempotencyKey != "" {
		env["FLO_IDEMPOTENCY_KEY"] = inv.IdempotencyKey
	}
	if inv.DryRun || dryRunCtx(ctx) {
		return core.Result{
			Outcome: core.OutcomeOK, Effect: core.EffectNone,
			Plan: &core.Plan{
				Capability: string(core.StepKindExec), Args: ex.Argv, IdempotencyKey: inv.IdempotencyKey,
				Rendered: map[string]any{"argv": ex.Argv, "cwd": ex.Cwd, "profile": inv.RunAs},
			},
		}, nil
	}
	timeout := ex.Timeout
	if timeout <= 0 {
		timeout = inv.Timeout
	}
	res, err := x.d.Commands.Run(ctx, core.CommandSpec{
		Argv: ex.Argv, Env: env, Cwd: ex.Cwd, Profile: inv.RunAs, Timeout: timeout, MaxOutput: ex.StdoutMax,
	})
	if err != nil {
		return failed(core.Errorf(core.CodeRunFailed, "cannot start %s: %v", firstArg(ex.Argv), err), core.EffectNone), nil
	}
	out := core.Result{Output: command.Raw(res), Raw: command.Raw(res), Truncated: res.Truncated, Calls: 1}
	switch {
	case ctx.Err() != nil:
		out.Err = core.Errorf(core.CodeRunCancelled, "%s was cancelled; partial output kept", firstArg(ex.Argv))
		out.Outcome, out.Effect = core.OutcomeFailed, core.EffectUnknown
	case res.TimedOut:
		out.Err = core.Errorf(core.CodeTimeout, "%s did not finish in %s; partial output kept", firstArg(ex.Argv), timeout)
		out.Outcome, out.Effect = core.OutcomeFailed, core.EffectUnknown
	case res.ExitCode != 0:
		out.Err = core.Errorf(core.CodeRunFailed, "%s exited %d", firstArg(ex.Argv), res.ExitCode).WithDetail("exit_code", res.ExitCode)
		out.Outcome, out.Effect = core.OutcomeFailed, core.EffectDone
	default:
		out.Outcome, out.Effect = core.OutcomeOK, core.EffectDone
	}
	return out, nil
}

// human parks the run on a whole-step decision (flow.md §5.3), and turns the
// completed approval into the step's output {decision, by, at, note}.
func (x *Dispatcher) human(inv core.Invocation) (core.Result, error) {
	h := inv.Human
	if h == nil {
		return core.Result{}, core.Errorf(core.CodeInternal, "a human step carries its rendered human")
	}
	if inv.DryRun {
		return core.Result{Outcome: core.OutcomeOK, Effect: core.EffectNone, Plan: &core.Plan{
			Capability: string(core.StepKindHuman), Args: map[string]any{"title": h.Title, "assignee": h.Assignee},
		}}, nil
	}
	if inv.Resume == nil {
		return core.Result{
			Outcome: core.OutcomePark, Effect: core.EffectNone,
			Park: &core.ApprovalRequest{
				Kind: core.ApprovalHuman, Title: h.Title, Surface: core.ApprovalSurfaceTLC,
				Verdicts: []core.Verdict{core.VerdictApprove, core.VerdictReject},
				Items:    []core.ApprovalItem{{ID: core.HumanItemID, Action: h.Title, Why: h.Description}},
				Timeout:  h.Timeout, OnTimeout: h.OnTimeout, Assignee: h.Assignee, Description: h.Description,
			},
		}, nil
	}
	it, ok := inv.Resume.Approval.Item(core.HumanItemID)
	if !ok || it.Decision == nil {
		return core.Result{}, core.Errorf(core.CodeInternal, "the approval of a human step has no decision")
	}
	d := it.Decision
	decision := "rejected"
	if d.Verdict.Approving() {
		decision = "approved"
	}
	return core.Result{
		Outcome: core.OutcomeOK, Effect: core.EffectDone,
		Output: map[string]any{"decision": decision, "by": d.By, "at": d.At.UTC().Format(time.RFC3339), "note": d.Reason},
	}, nil
}

func firstArg(argv []string) string {
	if len(argv) > 0 {
		return argv[0]
	}
	return "command"
}
