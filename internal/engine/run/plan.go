package run

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec"
	"hop.top/flo/spec/cel"
	specrun "hop.top/flo/spec/run"
)

// approvalNodePrefix names the built-in whose ancestors guard irreversible
// steps (flow.md V20).
const approvalNodePrefix = "flo/approval@"

// plan is a flow version compiled for execution: the expanded step list with
// every expression compiled. It is derived from the version's bytes on every
// turn, so nothing in it needs to survive a restart.
type plan struct {
	flow     *spec.Flow // with the expanded step list
	settings *specrun.Settings
	env      *cel.Env
	steps    []*stepPlan
	byID     map[string]*stepPlan
	returns  map[string]*cel.Template
	// staticSteps is the `steps` namespace: static fields of every step.
	staticSteps map[string]any
	// bundle is the flow bundle directory, the nodes/ layer of node lookup.
	bundle string
}

// stepPlan is one expanded step.
type stepPlan struct {
	step *spec.Step
	set  specrun.StepSettings
	pos  int // list order, the readiness tiebreak
	kind core.StepKind
	// deps is depends_on plus the implicit edges of results references;
	// data is the implicit part: the steps whose absence skips this one.
	deps, data []string
	// guards are the ancestor flo/approval@1 steps.
	guards []string

	when    *cel.Condition
	forEach *cel.Template
	with    *cel.Value
	exec    *cel.Value
	human   *cel.Value
	check   *cel.Value
	state   map[string]*cel.Template // outputs.state, by key (sorted at use)
	node    *core.NodeInfo
}

// loadPlan parses, expands and compiles the flow of a run. A flow the engine
// cannot run is an *core.Error: invalid_flow for a document problem,
// engine_feature for sub-flows and agent steps (phase 1), node_not_found for
// a node no layer has.
func (e *Executor) loadPlan(ctx context.Context, run core.Run) (*plan, error) {
	fv, err := e.d.Flows.GetVersion(ctx, run.Tenant, run.Flow.Name, run.Flow.Hash)
	if err != nil {
		return nil, err //nolint:wrapcheck // port error passes through
	}
	f, err := cel.ParseFlow(bytes.NewReader(fv.Source), fv.Name+".yaml")
	if err != nil {
		return nil, core.Wrap(core.CodeInvalidFlow, err)
	}
	set, err := specrun.Decode(f)
	if err != nil {
		return nil, core.Wrap(core.CodeInvalidFlow, err)
	}
	steps, err := expandSteps(f, fv.Bundle)
	if err != nil {
		return nil, core.Wrap(core.CodeInvalidFlow, err)
	}
	ef := *f
	ef.Steps = steps
	env, err := cel.EnvForFlow(&ef)
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err)
	}
	p := &plan{flow: &ef, settings: set, env: env, byID: map[string]*stepPlan{}, staticSteps: map[string]any{}, bundle: fv.Bundle}
	for i := range steps {
		sp, err := e.compileStep(ctx, p, run, &steps[i], i)
		if err != nil {
			return nil, err
		}
		p.steps = append(p.steps, sp)
		p.byID[sp.step.ID] = sp
	}
	for _, sp := range p.steps {
		sp.guards = p.guardsOf(sp)
	}
	if p.returns, err = compileReturns(env, f.Returns); err != nil {
		return nil, err
	}
	return p, nil
}

// expandSteps returns the flat step list. A flow without include or repeat
// blocks is already flat (and validated by the parser); spec.Expand is only
// called when there is something to expand, because it does not yet put the
// flow's declared state keys in scope and refuses every `state.<key>` read.
func expandSteps(f *spec.Flow, bundle string) ([]spec.Step, error) {
	blocks := slices.ContainsFunc(f.Steps, func(s spec.Step) bool { return s.IsBlock() })
	if !blocks {
		return f.Steps, nil
	}
	var dirs []string
	if bundle != "" {
		dirs = []string{bundle}
	}
	return spec.Expand(f, &spec.DirLocator{Dirs: dirs}) //nolint:wrapcheck // caller wraps
}

func compileReturns(env *cel.Env, returns map[string]string) (map[string]*cel.Template, error) {
	if len(returns) == 0 {
		return nil, nil //nolint:nilnil // no returns is a valid flow
	}
	out := make(map[string]*cel.Template, len(returns))
	for k, src := range returns {
		t, err := env.CompileTemplate(src)
		if err != nil {
			return nil, core.Errorf(core.CodeInvalidFlow, "returns.%s: %w", k, err)
		}
		out[k] = t
	}
	return out, nil
}

func (e *Executor) compileStep(ctx context.Context, p *plan, run core.Run, s *spec.Step, pos int) (*stepPlan, error) {
	bad := func(field string, err error) error {
		return core.Errorf(core.CodeInvalidFlow, "step %s: %s: %w", s.ID, field, err)
	}
	sp := &stepPlan{step: s, pos: pos}
	var err error
	if sp.set, err = specrun.DecodeStep(s); err != nil {
		return nil, core.Wrap(core.CodeInvalidFlow, err)
	}
	p.staticSteps[s.ID] = map[string]any{
		"id": s.ID, "kind": string(s.EffectiveKind()), "title": s.Title, "description": s.Description,
		"assignee": s.Assignee, "agent": s.Agent, "when": s.When,
	}
	switch {
	case s.IsUses() && !s.IsNode():
		return nil, core.Errorf(core.CodeEngineFeature, "step %s: sub-flow %q is not supported yet", s.ID, s.Uses).
			WithDetail("step", s.ID)
	case s.IsUses():
		sp.kind = core.StepKindNode
		info, err := e.d.Nodes.Resolve(ctx, core.NodeScope{Tenant: run.Tenant, Bundle: p.bundle}, s.Uses)
		if err != nil {
			return nil, err //nolint:wrapcheck // port error passes through
		}
		sp.node = &info
	case s.EffectiveKind() == spec.KindExec:
		if s.Exec == nil {
			return nil, core.Errorf(core.CodeInvalidFlow, "step %s: kind exec needs exec.argv", s.ID)
		}
		sp.kind = core.StepKindExec
	case s.EffectiveKind() == spec.KindHuman:
		sp.kind = core.StepKindHuman
	default:
		return nil, core.Errorf(core.CodeEngineFeature, "step %s: kind agent is not supported yet", s.ID).
			WithDetail("step", s.ID)
	}
	if sp.when, err = p.env.CompileCondition(s.When); err != nil {
		return nil, bad("when", err)
	}
	if s.ForEach != "" {
		if sp.forEach, err = p.env.CompileTemplate(s.ForEach); err != nil {
			return nil, bad("for_each", err)
		}
	}
	if len(s.With) > 0 {
		if sp.with, err = p.env.CompileValue("with", s.With); err != nil {
			return nil, bad("with", err)
		}
	}
	if sp.exec, err = compileExec(p.env, s); err != nil {
		return nil, bad("exec", err)
	}
	if sp.human, err = compileHuman(p.env, s); err != nil {
		return nil, bad("human", err)
	}
	if s.Check != nil {
		cv := map[string]any{"contract": s.Check.Contract, "eva_url": s.Check.EvaURL}
		if sp.check, err = p.env.CompileValue("check", cv); err != nil {
			return nil, bad("check", err)
		}
	}
	if sp.state, err = compileState(p.env, s); err != nil {
		return nil, bad("outputs", err)
	}
	sp.data = p.env.ImplicitDeps(s)
	sp.deps = slices.Clone(s.DependsOn)
	for _, id := range sp.data {
		if !slices.Contains(sp.deps, id) {
			sp.deps = append(sp.deps, id)
		}
	}
	return sp, nil
}

func compileExec(env *cel.Env, s *spec.Step) (*cel.Value, error) {
	if s.Exec == nil {
		return nil, nil //nolint:nilnil // not an exec step
	}
	argv := make([]any, len(s.Exec.Argv))
	for i, a := range s.Exec.Argv {
		argv[i] = a
	}
	v := map[string]any{"argv": argv, "cwd": s.Exec.Cwd, "env": s.Exec.Env}
	return env.CompileValue("exec", v) //nolint:wrapcheck // caller adds the field
}

func compileHuman(env *cel.Env, s *spec.Step) (*cel.Value, error) {
	if s.EffectiveKind() != spec.KindHuman || s.IsUses() {
		return nil, nil //nolint:nilnil // not a human step
	}
	assignee := s.Assignee
	if s.Human != nil && s.Human.Assignee != "" {
		assignee = s.Human.Assignee
	}
	v := map[string]any{"title": s.Title, "description": s.Description, "assignee": assignee}
	return env.CompileValue("human", v) //nolint:wrapcheck // caller adds the field
}

func compileState(env *cel.Env, s *spec.Step) (map[string]*cel.Template, error) {
	raw, ok := s.Outputs["state"]
	if !ok {
		return nil, nil //nolint:nilnil // the step writes no state
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("state: want a map of key to placeholder")
	}
	out := make(map[string]*cel.Template, len(m))
	for k, v := range m {
		str, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("state.%s: want a placeholder string", k)
		}
		t, err := env.CompileTemplate(str)
		if err != nil {
			return nil, fmt.Errorf("state.%s: %w", k, err)
		}
		out[k] = t
	}
	return out, nil
}

// guardsOf returns the flo/approval@1 steps among the ancestors of sp in the
// combined graph.
func (p *plan) guardsOf(sp *stepPlan) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		a := p.byID[id]
		if a == nil {
			return
		}
		if strings.HasPrefix(a.step.Uses, approvalNodePrefix) {
			out = append(out, id)
		}
		for _, d := range a.deps {
			walk(d)
		}
	}
	for _, d := range sp.deps {
		walk(d)
	}
	slices.Sort(out)
	return out
}
