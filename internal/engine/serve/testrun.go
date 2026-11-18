package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"hop.top/kit/go/runtime/sideeffect"
	"hop.top/kit/go/runtime/sideeffect/real"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/flowtest"
	httpnode "hop.top/flo/internal/nodes/http"
	"hop.top/flo/internal/placement"
	"hop.top/flo/spec"
	"hop.top/flo/spec/cel"
	specrun "hop.top/flo/spec/run"
)

// DefaultTestTimeout bounds one flow test.
const DefaultTestTimeout = 5 * time.Minute

// TestRequest asks for one test of a flow version.
type TestRequest struct {
	Flow string
	// Ref names the version (hash or document version); empty is the newest.
	Ref string
	// Mode is replay (the default) or record.
	Mode flowtest.Mode
	// Caller is the profile that tests; it must be allowed to use the flow's
	// run_as, as for a run.
	Caller string
	// Trigger is the trigger payload. Nil takes the recorded one, if any.
	Trigger map[string]any
	// Vars are the flow's vars as text, bound to their declared types.
	Vars map[string]string
	// State says where the flow state of the run comes from: "default" (the
	// declared defaults), "current" (a read-only copy of the engine's
	// committed state, taken at the start) or the path of a JSON or YAML seed
	// file. Empty is not explicit: a replay uses record/state.json when there
	// is one, else the defaults. A tests/ document's own state: wins.
	State string
	// Timeout bounds the test; zero is DefaultTestTimeout.
	Timeout time.Duration
}

// Layout of a bundle's record/: what `flo flow test --record` writes.
const (
	recordDir     = "record"
	cassetteDir   = "cassettes"
	triggerRecord = "trigger.json"
	plansRecord   = "plans.json"
	stateRecord   = "state.json"
)

// testPrep is a test request, resolved and checked: everything a run or a
// plan needs, read but nothing run or written.
type testPrep struct {
	req     TestRequest
	v       core.FlowVersion
	flow    *spec.Flow
	vars    map[string]any
	trigger map[string]any
	cases   []flowtest.Case
	set     *specrun.Settings
	choice  seedChoice
}

func (e *Engine) prepareTest(ctx context.Context, tenant core.TenantID, req TestRequest) (testPrep, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return testPrep{}, err //nolint:wrapcheck // *core.Error
	}
	if req.Mode == "" {
		req.Mode = flowtest.ModeReplay
	}
	if !req.Mode.Valid() {
		return testPrep{}, core.Errorf(core.CodeUsage, "test mode %q: want replay or record", req.Mode)
	}
	v, err := e.Services.Flows.GetVersion(ctx, tenant, req.Flow, req.Ref)
	if err != nil {
		return testPrep{}, err //nolint:wrapcheck // *core.Error
	}
	ck, err := e.Flows.Check(ctx, tenant, Source{File: v.Name + ".yaml", Bundle: v.Bundle, Data: v.Source})
	if err != nil {
		return testPrep{}, err
	}
	if err := ck.Report.Err(); err != nil {
		return testPrep{}, err //nolint:wrapcheck // *core.Error
	}
	f, err := cel.ParseFlow(bytes.NewReader(v.Source), v.Name+".yaml")
	if err != nil {
		return testPrep{}, core.Wrap(core.CodeInvalidFlow, err)
	}
	vars, err := specrun.ResolveVars(f, req.Vars)
	if err != nil {
		return testPrep{}, core.Wrap(core.CodeInvalidVar, err).WithSuggestion("--var name=value, typed per the flow's vars")
	}
	runAs := f.RunAs
	if runAs == "" {
		runAs = req.Caller
	}
	ok, err := e.Services.Grants.CanUseProfile(ctx, tenant, req.Caller, runAs)
	if err != nil {
		return testPrep{}, err //nolint:wrapcheck // *core.Error
	}
	if !ok {
		return testPrep{}, core.Errorf(core.CodeGrantDenied, "%s may not test this flow as %s", req.Caller, runAs).
			WithSuggestion("flo grant set " + req.Caller + " profile.use:" + runAs)
	}
	if req.Mode == flowtest.ModeRecord && v.Bundle == "" {
		return testPrep{}, core.Errorf(core.CodeUsage, "flow %s was applied from a single file: a recording needs a bundle directory to keep record/ in", v.Name).
			WithSuggestion("move the flow to <dir>/flow.yaml and apply the directory")
	}
	trigger := req.Trigger
	if trigger == nil && v.Bundle != "" {
		if trigger, err = readRecordedTrigger(v.Bundle); err != nil {
			return testPrep{}, err
		}
	}
	cases, err := flowtest.LoadCases(v.Bundle)
	if err != nil {
		return testPrep{}, err //nolint:wrapcheck // *core.Error
	}
	set, err := specrun.Decode(f)
	if err != nil {
		return testPrep{}, core.Wrap(core.CodeInvalidFlow, err)
	}
	for _, c := range cases {
		if err := flowtest.ValidateSeed(set.State, c.State, "tests/"+c.Name+": state"); err != nil {
			return testPrep{}, err //nolint:wrapcheck // *core.Error
		}
	}
	choice, err := e.resolveSeed(ctx, tenant, v, set.State, req)
	if err != nil {
		return testPrep{}, err
	}
	return testPrep{req: req, v: v, flow: f, vars: vars, trigger: trigger, cases: cases, set: set, choice: choice}, nil
}

// TestFlow runs one flow version in a sandbox engine (a throwaway data root
// with the same packs, grants and configuration) and stores the result.
//
// Nothing the flow does reaches the real stores: its run, state and outputs
// live in the sandbox, which is deleted afterwards. Every node that writes
// shared state or cannot be undone is not executed: its plan is recorded
// (kit sideeffect dry run). Reads and pure nodes run; their processes and HTTP
// calls (and the eva check) are answered from xrr cassettes in replay mode, and
// made for real and recorded in record mode. An approval is auto-approved:
// what a person would decide is not what is under test.
//
// The error is for faults and refusals (no such flow, the caller may not use
// run_as, a recording without a bundle); a flow that fails its test is a
// Result with Passed false.
func (e *Engine) TestFlow(ctx context.Context, tenant core.TenantID, req TestRequest) (flowtest.Result, error) {
	p, err := e.prepareTest(ctx, tenant, req)
	if err != nil {
		return flowtest.Result{}, err
	}
	req, v, vars, trigger, cases, set, choice := p.req, p.v, p.vars, p.trigger, p.cases, p.set, p.choice
	digest, err := flowtest.Digest(v.Bundle)
	if err != nil {
		return flowtest.Result{}, err //nolint:wrapcheck // *core.Error
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	scratch, err := os.MkdirTemp("", "flo-test-")
	if err != nil {
		return flowtest.Result{}, core.Wrap(core.CodeInternal, err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // the sandbox's throwaway root

	tapeDir := filepath.Join(scratch, "no-cassettes")
	if v.Bundle != "" {
		tapeDir = filepath.Join(v.Bundle, recordDir, cassetteDir)
	}
	tape, err := flowtest.NewTape(req.Mode, tapeDir)
	if err != nil {
		return flowtest.Result{}, core.Wrap(core.CodeInternal, err)
	}
	sb, err := e.openSandbox(ctx, scratch, tape)
	if err != nil {
		return flowtest.Result{}, err
	}
	defer func() { _ = sb.Close(context.WithoutCancel(ctx)) }()

	run, recs, err := sb.runOnce(ctx, tenant, req.Caller, v, vars, trigger, choice.seed)
	if err != nil {
		return flowtest.Result{}, err
	}

	steps, problems := flowtest.Evaluate(run, recs)
	var expects []flowtest.Expect
	var caseResults []flowtest.CaseResult
	held := 0
	for _, c := range cases {
		if c.State == nil {
			expects = append(expects, c.Expects...)
			continue
		}
		crun, crecs, err := sb.runOnce(ctx, tenant, req.Caller, v, vars, trigger, c.State)
		if err != nil {
			return flowtest.Result{}, err
		}
		csteps, cprobs := flowtest.Evaluate(crun, crecs)
		cheld, cexp := flowtest.Check(csteps, c.Expects)
		cprobs = append(cprobs, cexp...)
		for i := range cprobs {
			cprobs[i].Message = "case " + c.Name + ": " + cprobs[i].Message
		}
		problems = append(problems, cprobs...)
		held += cheld
		expects = append(expects, c.Expects...)
		caseResults = append(caseResults, flowtest.CaseResult{
			Name: c.Name, StateSource: flowtest.StateCase + c.Name, Run: string(crun.ID), Status: string(crun.Status), Passed: len(cprobs) == 0,
		})
	}
	bh, expProblems := flowtest.Check(steps, baseExpects(cases))
	held += bh
	problems = append(problems, expProblems...)
	for _, m := range tape.Misses() {
		problems = append(problems, flowtest.Problem{Kind: flowtest.ProblemCassette, Message: "no recording for " + m})
	}
	calls, replayed := tape.Stats()
	res := flowtest.Result{
		Schema: flowtest.SchemaVersion, Flow: v.Name, Version: v.Version, Hash: v.Hash, Digest: digest,
		Mode: req.Mode, StateSource: choice.source, Cases: caseResults, At: e.Config.Clock.Now().UTC(),
		Run: string(run.ID), Status: string(run.Status), Steps: steps, Problems: append([]flowtest.Problem{}, problems...),
		Expected: len(expects), Held: held, Calls: calls, Replayed: replayed,
	}
	res.Passed = len(res.Problems) == 0
	if req.Mode == flowtest.ModeRecord {
		if res.Recorded, err = writeRecord(v.Bundle, trigger, steps, fullState(set.State, choice.seed)); err != nil {
			return res, err
		}
		// The recording is part of the bundle the result is bound to.
		if res.Digest, err = flowtest.Digest(v.Bundle); err != nil {
			return res, err //nolint:wrapcheck // *core.Error
		}
	}
	if err := e.TestResults.Save(tenant, res); err != nil {
		return res, err //nolint:wrapcheck // *core.Error
	}
	return res, nil
}

// sandbox is a throwaway engine a test runs a flow in.
type sandbox struct {
	*Engine
	// state is the sandbox store's own; a run reads its seed instead.
	state core.StateStore
}

// openSandbox opens an engine over a new data root that sees what the real
// one does (packs, trust documents, configuration, grants) but writes
// elsewhere, with every process and request going through the tape and every
// dispatch a dry run.
func (e *Engine) openSandbox(ctx context.Context, root string, tape *flowtest.Tape) (*sandbox, error) {
	cfg := e.Config
	var liveExec sideeffect.Exec = real.Exec{}
	if cfg.Exec != nil {
		liveExec = cfg.Exec
	}
	var liveHTTP sideeffect.HTTP = real.NewHTTP(httpnode.NewClient())
	if cfg.HTTP != nil {
		liveHTTP = cfg.HTTP
	}
	cfg.Root = root
	cfg.PacksDir = e.Config.PacksDir
	cfg.TrustDirs = append([]string{filepath.Join(e.Config.Root, "trusts")}, e.Config.TrustDirs...)
	cfg.Exec, cfg.HTTP = tape.Exec(liveExec), tape.HTTP(liveHTTP)
	cfg.NoSync = true
	inner, err := Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	sb := &sandbox{Engine: inner, state: inner.Services.State}
	svc := inner.Services
	svc.Dispatcher = testDispatcher{svc.Dispatcher, cfg.Clock}
	svc.Runner = placement.New(svc.Dispatcher)
	svc.ApprovalSvc = approvedGuards{svc.ApprovalSvc}

	gs, err := e.Services.Grants.List(ctx, cfg.Tenant, "")
	if err != nil {
		_ = sb.Close(ctx)
		return nil, err //nolint:wrapcheck // *core.Error
	}
	for _, g := range gs {
		if _, err := svc.Grants.Set(ctx, cfg.Tenant, g.Profile, g.Capability); err != nil && !core.IsCode(err, core.CodeGrantProvisioned) {
			_ = sb.Close(ctx)
			return nil, err //nolint:wrapcheck // *core.Error
		}
	}
	return sb, nil
}

// runOnce stores the version in the sandbox, runs it and returns the run and
// its step records.
func (sb *sandbox) runOnce(ctx context.Context, tenant core.TenantID, caller string, v core.FlowVersion, vars, trigger, seed map[string]any) (core.Run, []core.StepRun, error) {
	svc := sb.Services
	svc.State = seededState{StateStore: sb.state, seed: seed}
	if _, _, err := svc.Flows.PutVersion(ctx, core.NewFlowVersion{
		Tenant: tenant, Name: v.Name, Version: v.Version, Hash: v.Hash, Source: v.Source, Bundle: v.Bundle, At: svc.Clock.Now(),
	}); err != nil {
		return core.Run{}, nil, err //nolint:wrapcheck // *core.Error
	}
	res, err := svc.Starter.Start(ctx, core.StartRequest{
		Tenant: tenant, Flow: v.Name, Version: v.Hash, Vars: vars, StartedBy: caller,
		Trigger: core.TriggerInfo{Kind: specrun.TriggerManual, Payload: trigger},
	})
	if err != nil {
		return core.Run{}, nil, err //nolint:wrapcheck // *core.Error
	}
	if err := sb.Executor.Advance(ctx, tenant, res.Run.ID); err != nil {
		return core.Run{}, nil, err //nolint:wrapcheck // *core.Error
	}
	run, err := svc.Runs.GetRun(ctx, tenant, res.Run.ID)
	if err != nil {
		return core.Run{}, nil, err //nolint:wrapcheck // *core.Error
	}
	recs, err := svc.Runs.ListSteps(ctx, tenant, res.Run.ID)
	if err != nil {
		return core.Run{}, nil, err //nolint:wrapcheck // *core.Error
	}
	return run, recs, nil
}

// testDispatcher makes every dispatch a dry run: nodes that write shared state
// or cannot be undone return their plan (the dispatcher's own rule, node.md §8),
// and what a person or a recipient would have decided is filled in: an approval
// approves every item, a human step approves.
type testDispatcher struct {
	core.Dispatcher
	clock core.Clock
}

func (d testDispatcher) Dispatch(ctx context.Context, inv core.Invocation) (core.Result, error) {
	inv.DryRun = true
	res, err := d.Dispatcher.Dispatch(sideeffect.WithDryRun(ctx, true), inv)
	if err != nil || res.Outcome != core.OutcomeOK {
		return res, err //nolint:wrapcheck // the dispatcher's own result
	}
	switch {
	case inv.Kind == core.StepKindHuman && res.Output == nil:
		res.Output = map[string]any{"decision": "approved", "by": "flo-test", "at": d.clock.Now().UTC().Format(time.RFC3339), "note": ""}
	case inv.Kind == core.StepKindNode && inv.Node != nil && inv.Node.Node == approvalNode && res.Output == nil:
		items, _ := inv.In["items"].([]any)
		if items == nil {
			items = []any{}
		}
		res.Output = map[string]any{"approved": items, "rejected": []any{}, "corrected": []any{}, "auto": []any{}, "reason": map[string]any{}}
	}
	return res, nil
}

// approvalNode is the built-in an approval step runs.
const approvalNode = "flo/approval@1"

// approvedGuards answers the guard of an irreversible step as satisfied: in a
// test the approvals are auto-approved, and the step does not run anyway. That
// a guard exists is checked statically when the flow is validated.
type approvedGuards struct{ core.ApprovalService }

func (approvedGuards) GuardSatisfied(context.Context, core.GuardQuery) (bool, error) {
	return true, nil
}

func readRecordedTrigger(bundle string) (map[string]any, error) {
	b, err := os.ReadFile(filepath.Join(bundle, recordDir, triggerRecord)) //nolint:gosec // inside the bundle
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // no recording, no payload
	}
	if err != nil {
		return nil, core.Errorf(core.CodeUsage, "%s/%s: %v", recordDir, triggerRecord, err)
	}
	v, err := core.DecodeJSONValue(b)
	if err != nil {
		return nil, core.Errorf(core.CodeUsage, "%s/%s: not JSON: %v", recordDir, triggerRecord, err)
	}
	m, _ := v.(map[string]any)
	return m, nil
}

// writeRecord keeps the trigger payload and the plans of the writes a
// recording did not execute, beside the cassettes. It returns what it wrote,
// relative to the bundle.
func writeRecord(bundle string, trigger map[string]any, steps []flowtest.Step, state map[string]any) ([]string, error) {
	dir := filepath.Join(bundle, recordDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, core.Wrap(core.CodeInternal, err)
	}
	type plan struct {
		Step  string `json:"step"`
		Index *int   `json:"index,omitempty"`
		Node  string `json:"node,omitempty"`
		Plan  any    `json:"plan"`
	}
	plans := []plan{}
	for _, s := range steps {
		if s.Planned {
			plans = append(plans, plan{Step: s.ID, Index: s.Index, Node: s.Node, Plan: s.Plan})
		}
	}
	var wrote []string
	for name, v := range map[string]any{triggerRecord: trigger, plansRecord: plans, stateRecord: state} {
		if name == triggerRecord && trigger == nil {
			continue
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, core.Wrap(core.CodeInternal, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o600); err != nil {
			return nil, core.Wrap(core.CodeInternal, err)
		}
		wrote = append(wrote, filepath.ToSlash(filepath.Join(recordDir, name)))
	}
	wrote = append(wrote, filepath.ToSlash(filepath.Join(recordDir, cassetteDir))+"/")
	slices.Sort(wrote)
	return wrote, nil
}
