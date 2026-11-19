// Package dispatch routes an invocation to what runs it (core.Dispatcher):
// a built-in, a declared command or HTTP node, a plugin, or a core exec or
// human step. Around the call it validates the input against the node spec,
// enforces dry run, draws on rate-limit buckets and checks the output
// against its schema. It also holds the NodeRegistry over the spec layers.
package dispatch

import (
	"context"
	"encoding/json"
	"reflect"

	"hop.top/kit/go/runtime/sideeffect"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/command"
	httpnode "hop.top/flo/internal/nodes/http"
	"hop.top/flo/internal/nodes/plugin"
	"hop.top/flo/internal/nodes/ratelimit"
	"hop.top/flo/internal/nodes/render"
	"hop.top/flo/internal/nodes/schema"
	"hop.top/flo/spec"
	"hop.top/flo/spec/node"
)

// Deps are what the dispatcher runs on.
type Deps struct {
	Builtins core.BuiltinRegistry
	// Commands runs every process: declared command nodes and core exec
	// steps.
	Commands core.CommandRunner
	// HTTP sends declared HTTP nodes' requests (NewHTTPClient's client under
	// real.NewHTTP in production).
	HTTP    sideeffect.HTTP
	Secrets core.Secrets
	Plugins *plugin.Registry
	// Limits holds the rate-limit buckets; nil makes a private set.
	Limits *ratelimit.Limiter
	Clock  core.Clock
	// StepRoot is where command nodes make their per-attempt scratch
	// directories; empty is the system temp directory.
	StepRoot string
}

// Dispatcher is the core.Dispatcher.
type Dispatcher struct {
	d       Deps
	command *command.Executor
	http    *httpnode.Executor
	plugin  *plugin.Executor
}

// New returns a Dispatcher.
func New(d Deps) *Dispatcher {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	if d.Limits == nil {
		d.Limits = ratelimit.New(d.Clock)
	}
	if d.Plugins == nil {
		d.Plugins = plugin.NewRegistry()
	}
	return &Dispatcher{
		d:       d,
		command: &command.Executor{Commands: d.Commands, StepRoot: d.StepRoot},
		http:    &httpnode.Executor{HTTP: d.HTTP, Secrets: d.Secrets, Clock: d.Clock},
		plugin:  &plugin.Executor{Plugins: d.Plugins},
	}
}

// Dispatch implements core.Dispatcher. A step failure is a Result with
// OutcomeFailed; the error return is for faults of the engine.
func (x *Dispatcher) Dispatch(ctx context.Context, inv core.Invocation) (core.Result, error) {
	if err := ctx.Err(); err != nil {
		return failed(core.Errorf(core.CodeRunCancelled, "cancelled before the step started"), core.EffectNone), nil
	}
	switch inv.Kind {
	case core.StepKindNode:
		return x.node(ctx, inv)
	case core.StepKindExec:
		return x.exec(ctx, inv)
	case core.StepKindHuman:
		return x.human(inv)
	case core.StepKindAgent, core.StepKindFlow:
		return failed(core.Errorf(core.CodeEngineFeature, "steps of kind %s are not available in this engine", inv.Kind), core.EffectNone), nil
	}
	return core.Result{}, core.Errorf(core.CodeInternal, "unknown step kind %q", inv.Kind)
}

func (x *Dispatcher) node(ctx context.Context, inv core.Invocation) (core.Result, error) {
	s := inv.Node
	if s == nil {
		return core.Result{}, core.Errorf(core.CodeInternal, "a node step carries its resolved spec")
	}
	in, ierr := schema.ApplyInputs(s.Inputs, inv.In)
	if ierr != nil {
		return failed(ierr, core.EffectNone), nil
	}
	inv.In = in

	if (inv.DryRun || sideeffect.IsDryRun(ctx)) && !ExecutesInDryRun(s.SideEffect) {
		return x.dryRun(inv)
	}
	if rl := s.RateLimit; rl != nil {
		per, perr := spec.ParseDuration(rl.Per)
		if perr != nil {
			return failed(core.Errorf(core.CodeInvalidNode, "%s: rate_limit.per: %v", s.Node, perr), core.EffectNone), nil
		}
		if lerr := x.d.Limits.Take(ctx, inv.Tenant, ratelimit.Limit{Bucket: rl.Bucket, Max: rl.Max, Per: per}, render.Timeout(inv)); lerr != nil {
			if ce := asError(lerr); ce != nil {
				return failed(ce, core.EffectNone), nil
			}
			return failed(core.Errorf(core.CodeRunCancelled, "cancelled waiting for rate limit bucket %q", rl.Bucket), core.EffectNone), nil
		}
	}

	res, err := x.run(ctx, inv)
	if err != nil {
		return res, err
	}
	if res.Outcome == core.OutcomeOK {
		if oerr := schema.CheckOutput(s, res.Output); oerr != nil {
			res.Outcome, res.Err = core.OutcomeFailed, oerr
		}
	}
	if res.Cost.IsZero() && s.Cost != nil && (s.Kind == node.KindCommand || s.Kind == node.KindHTTP) {
		res.Cost = core.Cost{USD: s.Cost.Estimate.USD, Tokens: s.Cost.Estimate.Tokens}
	}
	return res, nil
}

func (x *Dispatcher) run(ctx context.Context, inv core.Invocation) (core.Result, error) {
	switch inv.Node.Kind {
	case node.KindBuiltin:
		b, ok := x.d.Builtins.Lookup(inv.Node.Node)
		if !ok {
			return core.Result{}, core.Errorf(core.CodeNodeNotFound, "built-in %s is not registered", inv.Node.Node)
		}
		if x.d.HTTP != nil {
			ctx = core.WithHTTP(ctx, x.d.HTTP)
		}
		return executeBuiltin(ctx, b, inv)
	case node.KindCommand:
		return x.command.Execute(ctx, inv) //nolint:wrapcheck // the executor's own result
	case node.KindHTTP:
		return x.http.Execute(ctx, inv) //nolint:wrapcheck // the executor's own result
	case node.KindPlugin:
		return x.plugin.Execute(ctx, inv) //nolint:wrapcheck // the executor's own result
	}
	return core.Result{}, core.Errorf(core.CodeInvalidNode, "%s: kind %q is not dispatchable", inv.Node.Node, inv.Node.Kind)
}

// dryRun answers a write-shared or irreversible node without executing it
// (node.md §8): the Plan of what it would do, and an output taken from
// outputs.dry_run, else from the examples, else null.
func (x *Dispatcher) dryRun(inv core.Invocation) (core.Result, error) {
	s := inv.Node
	var rendered any
	switch s.Kind {
	case node.KindCommand:
		r, perr := x.command.Plan(inv)
		if perr != nil {
			return failed(perr, core.EffectNone), nil
		}
		rendered = r
	case node.KindHTTP:
		r, perr := x.http.Plan(inv)
		if perr != nil {
			return failed(perr, core.EffectNone), nil
		}
		rendered = r
	case node.KindPlugin:
		rendered = x.plugin.Plan(inv)
	default:
		rendered = map[string]any{"builtin": s.Node, "in": inv.In}
	}
	out, derr := dryOutput(s, inv)
	if derr != nil {
		return failed(derr, core.EffectNone), nil
	}
	return core.Result{
		Outcome: core.OutcomeOK, Output: out, Effect: core.EffectNone,
		Plan: &core.Plan{Capability: s.Node, Args: inv.In, IdempotencyKey: inv.IdempotencyKey, Rendered: rendered},
	}, nil
}

// dryOutput picks the output of a dry run: outputs.dry_run (templates over
// in and config rendered), else the output of the example whose input is
// this one, else the first example's output, else null.
func dryOutput(s *node.Spec, inv core.Invocation) (any, *core.Error) {
	if s.Outputs.DryRun != nil {
		env, err := render.Shared()
		if err != nil {
			return nil, core.Wrap(core.CodeInternal, err)
		}
		v, rerr := env.Value(s.Outputs.DryRun, render.Scope{In: inv.In, Config: inv.Config, Run: inv.Run}.Activation())
		if rerr != nil {
			return nil, render.Fail("outputs.dry_run", rerr)
		}
		return v, nil
	}
	var first any
	haveFirst := false
	for _, ex := range s.Examples {
		if ex.Out == nil {
			continue
		}
		if !haveFirst {
			first, haveFirst = ex.Out, true
		}
		if sameJSON(ex.In, inv.In) {
			return ex.Out, nil
		}
	}
	return first, nil
}

func sameJSON(a, b any) bool {
	var x, y any
	ab, aerr := json.Marshal(a)
	bb, berr := json.Marshal(b)
	if aerr != nil || berr != nil || json.Unmarshal(ab, &x) != nil || json.Unmarshal(bb, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func dryRunCtx(ctx context.Context) bool { return sideeffect.IsDryRun(ctx) }

func failed(e *core.Error, eff core.Effect) core.Result {
	return core.Result{Outcome: core.OutcomeFailed, Err: e, Effect: eff}
}

// asError returns err as *core.Error when it is one.
func asError(err error) *core.Error {
	if ce, ok := err.(*core.Error); ok { //nolint:errorlint // the limiter returns its *core.Error unwrapped
		return ce
	}
	return nil
}

// executeBuiltin runs a built-in and returns as soon as ctx is done, even if
// the built-in is still working (the executor waits for the dispatch).
func executeBuiltin(ctx context.Context, b core.BuiltinNode, inv core.Invocation) (core.Result, error) {
	type answer struct {
		res core.Result
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := b.Execute(ctx, inv)
		done <- answer{res, err}
	}()
	select {
	case a := <-done:
		return a.res, a.err
	case <-ctx.Done():
		return failed(core.Errorf(core.CodeRunCancelled, "%s: cancelled", inv.Node.Node), core.EffectUnknown), nil
	}
}
