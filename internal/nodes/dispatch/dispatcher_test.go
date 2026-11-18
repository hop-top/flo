package dispatch_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/kit/go/ai/ext"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/runtime/sideeffect"
	"hop.top/kit/go/runtime/sideeffect/real"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/nodes/builtin"
	"hop.top/flo/internal/nodes/dispatch"
	httpnode "hop.top/flo/internal/nodes/http"
	"hop.top/flo/internal/nodes/plugin"
	"hop.top/flo/internal/nodes/render"
	"hop.top/flo/spec/node"
)

func spec(t *testing.T, y string) *node.Spec {
	t.Helper()
	s, err := node.Decode([]byte(y), "n.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type rig struct {
	fx   *coretest.Fixture
	d    *dispatch.Dispatcher
	plug *plugin.Registry
}

func newRig(t *testing.T) *rig {
	t.Helper()
	fx := coretest.New()
	bi := builtin.NewRegistry()
	if err := builtin.RegisterStd(bi, builtin.Deps{Outputs: fx.DB.Outputs, IDs: fx.IDs, Clock: fx.Clock}); err != nil {
		t.Fatal(err)
	}
	plug := plugin.NewRegistry()
	return &rig{fx: fx, plug: plug, d: dispatch.New(dispatch.Deps{
		Builtins: bi, Commands: fx.Commands, HTTP: real.NewHTTP(httpnode.NewClient()), Secrets: fx.Secrets,
		Plugins: plug, Clock: fx.Clock,
	})}
}

func inv(s *node.Spec, in map[string]any) core.Invocation {
	return core.Invocation{
		Tenant: "default", Run: core.RunContext{ID: "run_1", Flow: "f", Version: "1"}, StepID: "s",
		Kind: core.StepKindNode, Node: s, In: in, RunAs: "noor", Attempt: 1,
	}
}

const readCmd = `
node: gh/list@1
kind: command
argv: [gh, api, "repos/{{ in.repo }}/releases"]
inputs:
  repo: { type: string, required: true }
  limit: { type: integer, default: 5, minimum: 1 }
outputs:
  from: stdout
  format: json
  schema: { type: array, items: { type: object, required: [tag] } }
side_effect: read
privacy: cloud
capabilities: [vcs.read]
cost: { class: low, estimate: { usd: 0.001 } }
`

func TestCommandNodeThroughDispatcher(t *testing.T) {
	skipWithoutInAlias(t)
	r := newRig(t)
	r.fx.Commands.Reply([]string{"gh", "api"}, core.CommandResult{Stdout: []byte(`[{"tag":"v1"}]`)})
	res, err := r.d.Dispatch(context.Background(), inv(spec(t, readCmd), map[string]any{"repo": "a/b"}))
	if err != nil || res.Outcome != core.OutcomeOK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	call := r.fx.Commands.Calls()[0]
	if !slices.Equal(call.Argv, []string{"gh", "api", "repos/a/b/releases"}) || call.Profile != "noor" {
		t.Fatalf("call = %+v", call)
	}
	if res.Cost.USD != 0.001 {
		t.Fatalf("declared nodes report their fixed estimate: %+v", res.Cost)
	}
}

func TestInputInvalidNeverRuns(t *testing.T) {
	skipWithoutInAlias(t)
	r := newRig(t)
	for name, in := range map[string]map[string]any{
		"missing": {}, "bad type": {"repo": 1}, "below minimum": {"repo": "a", "limit": 0}, "unknown": {"repo": "a", "nope": true},
	} {
		res, err := r.d.Dispatch(context.Background(), inv(spec(t, readCmd), in))
		if err != nil || res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeInputInvalid || res.Effect != core.EffectNone {
			t.Fatalf("%s: res=%+v err=%v", name, res, err)
		}
	}
	if n := len(r.fx.Commands.Calls()); n != 0 {
		t.Fatalf("a rejected input must not reach the process: %d calls", n)
	}
}

func TestOutputInvalid(t *testing.T) {
	skipWithoutInAlias(t)
	r := newRig(t)
	r.fx.Commands.Reply([]string{"gh", "api"}, core.CommandResult{Stdout: []byte(`[{"nope":1}]`)})
	res, _ := r.d.Dispatch(context.Background(), inv(spec(t, readCmd), map[string]any{"repo": "a/b"}))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeOutputInvalid || res.Output == nil || res.Raw == nil {
		t.Fatalf("res = %+v", res)
	}
	if res.Err.Retryable() {
		t.Fatal("output_invalid is permanent")
	}
}

const irreversible = `
node: mail/send@1
kind: command
argv: [inbox, send, "{{ in.to }}"]
inputs: { to: { type: string, required: true } }
outputs:
  from: stdout
  format: json
  dry_run: { sent: false, to: "{{ in.to }}" }
  schema: { type: object, required: [sent] }
side_effect: irreversible
privacy: local
capabilities: [mail.send]
`

func TestDryRunNeverExecutesIrreversibleAndReturnsPlan(t *testing.T) {
	skipWithoutInAlias(t)
	r := newRig(t)
	i := inv(spec(t, irreversible), map[string]any{"to": "a@b.c"})
	i.DryRun = true
	i.IdempotencyKey = "k1"
	res, err := r.d.Dispatch(context.Background(), i)
	if err != nil || res.Outcome != core.OutcomeOK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if n := len(r.fx.Commands.Calls()); n != 0 {
		t.Fatalf("an irreversible node must not execute under dry run: %d calls", n)
	}
	plan := res.Plan
	if plan == nil || plan.Capability != "mail/send@1" || plan.IdempotencyKey != "k1" {
		t.Fatalf("plan = %+v", plan)
	}
	if argv := plan.Rendered.(map[string]any)["argv"].([]string); !slices.Equal(argv, []string{"inbox", "send", "a@b.c"}) {
		t.Fatalf("the plan shows the rendered argv: %v", argv)
	}
	if out := res.Output.(map[string]any); out["sent"] != false || out["to"] != "a@b.c" {
		t.Fatalf("outputs.dry_run: %v", out)
	}
	if res.Effect != core.EffectNone {
		t.Fatalf("effect = %s", res.Effect)
	}
	// the kit dry-run flag on the context has the same effect
	ctx := sideeffect.WithDryRun(context.Background(), true)
	if _, err := r.d.Dispatch(ctx, inv(spec(t, irreversible), map[string]any{"to": "a@b.c"})); err != nil || len(r.fx.Commands.Calls()) != 0 {
		t.Fatalf("ctx dry run executed: %d calls err=%v", len(r.fx.Commands.Calls()), err)
	}
}

func TestDryRunOutputFromExamplesOrNull(t *testing.T) {
	skipWithoutInAlias(t)
	r := newRig(t)
	base := `
node: tlc/create@1
kind: command
argv: [tlc, create, "{{ in.title }}"]
inputs: { title: { type: string, required: true } }
outputs: { from: stdout, format: json, schema: { type: object } }
side_effect: write-shared
privacy: local
capabilities: [tasks.write]
`
	withExamples := base + `examples:
  - { name: other, in: { title: other }, out: { id: 1 } }
  - { name: mine, in: { title: mine }, out: { id: 2 } }
`
	i := inv(spec(t, withExamples), map[string]any{"title": "mine"})
	i.DryRun = true
	res, _ := r.d.Dispatch(context.Background(), i)
	if res.Output.(map[string]any)["id"] != 2 && res.Output.(map[string]any)["id"] != int(2) {
		t.Fatalf("the example whose input matches: %v", res.Output)
	}
	i = inv(spec(t, withExamples), map[string]any{"title": "unlisted"})
	i.DryRun = true
	res, _ = r.d.Dispatch(context.Background(), i)
	if res.Output.(map[string]any)["id"] != 1 && res.Output.(map[string]any)["id"] != int(1) {
		t.Fatalf("else the first example: %v", res.Output)
	}
	i = inv(spec(t, base), map[string]any{"title": "x"})
	i.DryRun = true
	res, _ = r.d.Dispatch(context.Background(), i)
	if res.Outcome != core.OutcomeOK || res.Output != nil || res.Plan == nil {
		t.Fatalf("otherwise null: %+v", res)
	}
	if len(r.fx.Commands.Calls()) != 0 {
		t.Fatal("nothing executed")
	}
}

func TestDryRunStillRunsReads(t *testing.T) {
	skipWithoutInAlias(t)
	r := newRig(t)
	r.fx.Commands.Reply([]string{"gh", "api"}, core.CommandResult{Stdout: []byte(`[]`)})
	i := inv(spec(t, readCmd), map[string]any{"repo": "a/b"})
	i.DryRun = true
	res, _ := r.d.Dispatch(context.Background(), i)
	if res.Outcome != core.OutcomeOK || res.Plan != nil || len(r.fx.Commands.Calls()) != 1 {
		t.Fatalf("reads execute under dry run: %+v", res)
	}
}

func TestDryRunHTTPSendsNothing(t *testing.T) {
	skipWithoutInAlias(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer srv.Close()
	r := newRig(t)
	s := spec(t, "node: x/post@1\nkind: http\nrequest: { method: POST, url: '"+srv.URL+"', json: { a: '{{ in.a }}' }, auth: { secret: k, scheme: bearer } }\ninputs: { a: { type: string, required: true } }\noutputs: { schema: {} }\nside_effect: write-shared\nprivacy: local\ncapabilities: [tasks.write]\n")
	i := inv(s, map[string]any{"a": "v"})
	i.DryRun = true
	res, _ := r.d.Dispatch(context.Background(), i)
	if res.Outcome != core.OutcomeOK || atomic.LoadInt32(&hits) != 0 || res.Plan == nil {
		t.Fatalf("hits=%d res=%+v", hits, res)
	}
	rendered := res.Plan.Rendered.(map[string]any)
	if rendered["method"] != "POST" || rendered["body"] != `{"a":"v"}` {
		t.Fatalf("rendered = %v", rendered)
	}
}

func TestBuiltinThroughDispatcher(t *testing.T) {
	skipWithoutInAlias(t)
	r := newRig(t)
	spec, err := builtin.Spec("flo/dedupe@1")
	if err != nil {
		t.Fatal(err)
	}
	i := inv(spec, map[string]any{"items": []any{"a", "a", "b"}, "key": "item"})
	i.Kind = core.StepKindNode
	res, derr := r.d.Dispatch(context.Background(), i)
	if derr != nil || res.Outcome != core.OutcomeOK {
		t.Fatalf("res=%+v err=%v", res, derr)
	}
	if keys := res.Output.(map[string]any)["keys"].([]any); len(keys) != 2 {
		t.Fatalf("keys = %v", keys)
	}
	// defaults were applied before the built-in ran: max is 10000
	bad := inv(spec, map[string]any{"items": "not a list", "key": "item"})
	if res, _ := r.d.Dispatch(context.Background(), bad); res.Err.Code != core.CodeInputInvalid {
		t.Fatalf("res = %+v", res)
	}
}

// ---- plugins ----

type testPlugin struct {
	specs []*node.Spec
	reply any
	got   *plugin.Call
}

func (p *testPlugin) Meta() ext.Metadata           { return ext.Metadata{Name: "test"} }
func (p *testPlugin) Capabilities() ext.Capability { return ext.CapRegistry }
func (p *testPlugin) Init(context.Context) error   { return nil }
func (p *testPlugin) Close() error                 { return nil }
func (p *testPlugin) Specs() []*node.Spec          { return p.specs }
func (p *testPlugin) Call(_ context.Context, c plugin.Call) (plugin.Reply, error) {
	p.got = &c
	return plugin.Reply{Output: p.reply}, nil
}

const pluginSpec = `
node: calendar/summary@1
kind: plugin
inputs:
  calendar: { type: string, required: true }
  days: { type: integer, default: 7 }
outputs:
  schema: { type: object, required: [count], properties: { count: { type: integer } } }
side_effect: read
privacy: local
capabilities: [calendar.read]
`

func TestPluginSchemaBothSides(t *testing.T) {
	skipWithoutInAlias(t)
	r := newRig(t)
	s := spec(t, pluginSpec)
	p := &testPlugin{specs: []*node.Spec{s}, reply: map[string]any{"count": 3}}
	if err := r.plug.RegisterNode(p); err != nil {
		t.Fatal(err)
	}

	res, _ := r.d.Dispatch(context.Background(), inv(s, map[string]any{"calendar": "work"}))
	if res.Outcome != core.OutcomeOK || p.got == nil || p.got.In["days"] != 7 {
		t.Fatalf("defaults reach the plugin: res=%+v got=%+v", res, p.got)
	}

	// input side: refused before the call
	p.got = nil
	res, _ = r.d.Dispatch(context.Background(), inv(s, map[string]any{"days": 1}))
	if res.Err == nil || res.Err.Code != core.CodeInputInvalid || p.got != nil {
		t.Fatalf("a bad input must not reach the plugin: res=%+v", res)
	}

	// output side: checked after the call
	p.reply = map[string]any{"count": "three"}
	res, _ = r.d.Dispatch(context.Background(), inv(s, map[string]any{"calendar": "work"}))
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeOutputInvalid {
		t.Fatalf("res = %+v", res)
	}
}

// ---- rate limits ----

func limited(name string) string {
	return `
node: gh/` + name + `@1
kind: command
argv: [gh, ` + name + `]
inputs: {}
outputs: { from: stdout, format: json, schema: {} }
side_effect: read
privacy: cloud
capabilities: [vcs.read]
timeout: 1s
rate_limit: { bucket: vcs, max: 2, per: 1h }
`
}

func TestSharedBucketAcrossNodes(t *testing.T) {
	skipWithoutInAlias(t)
	r := newRig(t)
	r.fx.Commands.Reply([]string{"gh"}, core.CommandResult{Stdout: []byte(`{}`)})
	a, b := spec(t, limited("a")), spec(t, limited("b"))
	for _, s := range []*node.Spec{a, b} {
		if res, _ := r.d.Dispatch(context.Background(), inv(s, nil)); res.Outcome != core.OutcomeOK {
			t.Fatalf("within the quota: %+v", res.Err)
		}
	}
	res, err := r.d.Dispatch(context.Background(), inv(a, nil))
	if err != nil || res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeProviderRateLimited || !res.Err.Retryable() || res.Effect != core.EffectNone {
		t.Fatalf("a third call, on either node, hits the shared bucket: res=%+v err=%v", res, err)
	}
	if n := len(r.fx.Commands.Calls()); n != 2 {
		t.Fatalf("the limited call must not run: %d", n)
	}
	// the window slides
	r.fx.Clock.Advance(time.Hour)
	if res, _ := r.d.Dispatch(context.Background(), inv(b, nil)); res.Outcome != core.OutcomeOK {
		t.Fatalf("after the window: %+v", res.Err)
	}
}

// ---- core kinds ----

func TestCoreExecStep(t *testing.T) {
	r := newRig(t)
	r.fx.Commands.Reply([]string{"make"}, core.CommandResult{Stdout: []byte("built"), Duration: 4 * time.Millisecond})
	i := core.Invocation{Kind: core.StepKindExec, RunAs: "noor", IdempotencyKey: "k", Exec: &core.ExecInput{Argv: []string{"make", "build"}, Cwd: "/w", Env: map[string]string{"A": "1"}}}
	res, err := r.d.Dispatch(context.Background(), i)
	if err != nil || res.Outcome != core.OutcomeOK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if out := res.Output.(map[string]any); out["stdout"] != "built" || out["exit_code"] != int64(0) {
		t.Fatalf("output is the exec result object: %v", out)
	}
	call := r.fx.Commands.Calls()[0]
	if call.Profile != "noor" || call.Cwd != "/w" || call.Env["A"] != "1" || call.Env["FLO_IDEMPOTENCY_KEY"] != "k" {
		t.Fatalf("call = %+v", call)
	}
	r.fx.Commands.Reply([]string{"fail"}, core.CommandResult{ExitCode: 2})
	res, _ = r.d.Dispatch(context.Background(), core.Invocation{Kind: core.StepKindExec, Exec: &core.ExecInput{Argv: []string{"fail"}}})
	if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeRunFailed || res.Output == nil {
		t.Fatalf("non-zero exit fails the step, output kept: %+v", res)
	}
	// dry run: a core exec is treated as write-shared
	d := core.Invocation{Kind: core.StepKindExec, DryRun: true, Exec: &core.ExecInput{Argv: []string{"make"}}}
	n := len(r.fx.Commands.Calls())
	res, _ = r.d.Dispatch(context.Background(), d)
	if res.Plan == nil || len(r.fx.Commands.Calls()) != n {
		t.Fatalf("dry run: %+v", res)
	}
}

func TestHumanStepParksAndResumes(t *testing.T) {
	r := newRig(t)
	i := core.Invocation{Kind: core.StepKindHuman, Human: &core.HumanInput{Title: "ship it?", Assignee: "noor", Timeout: time.Hour, OnTimeout: "reject"}}
	res, err := r.d.Dispatch(context.Background(), i)
	if err != nil || res.Outcome != core.OutcomePark || res.Park == nil {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if res.Park.Kind != core.ApprovalHuman || len(res.Park.Items) != 1 || res.Park.Items[0].ID != core.HumanItemID || res.Park.OnTimeout != "reject" {
		t.Fatalf("park = %+v", res.Park)
	}
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	i.Resume = &core.Resume{Approval: core.Approval{Items: []core.ApprovalItem{{ID: core.HumanItemID, Decision: &core.ItemDecision{Verdict: core.VerdictApprove, By: "jad", At: at, Reason: "ok"}}}}}
	res, _ = r.d.Dispatch(context.Background(), i)
	out, _ := res.Output.(map[string]any)
	if res.Outcome != core.OutcomeOK || out["decision"] != "approved" || out["by"] != "jad" || out["note"] != "ok" || out["at"] != "2026-11-18T12:00:00Z" {
		t.Fatalf("res = %+v", res)
	}
	i.Resume.Approval.Items[0].Decision.Verdict = core.VerdictReject
	if res, _ = r.d.Dispatch(context.Background(), i); res.Output.(map[string]any)["decision"] != "rejected" {
		t.Fatalf("res = %+v", res)
	}
}

func TestAgentAndFlowKindsRefused(t *testing.T) {
	r := newRig(t)
	for _, k := range []core.StepKind{core.StepKindAgent, core.StepKindFlow} {
		res, err := r.d.Dispatch(context.Background(), core.Invocation{Kind: k})
		if err != nil || res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodeEngineFeature {
			t.Fatalf("%s: res=%+v err=%v", k, res, err)
		}
	}
	if _, err := r.d.Dispatch(context.Background(), core.Invocation{Kind: "weird"}); !core.IsCode(err, core.CodeInternal) {
		t.Fatalf("an unknown kind is an engine fault: %v", err)
	}
}

func TestCancelledContext(t *testing.T) {
	skipWithoutInAlias(t)
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := r.d.Dispatch(ctx, inv(spec(t, readCmd), map[string]any{"repo": "a/b"}))
	if err != nil || res.Err == nil || res.Err.Code != core.CodeRunCancelled || len(r.fx.Commands.Calls()) != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestEngineFaultsAreErrors(t *testing.T) {
	r := newRig(t)
	if _, err := r.d.Dispatch(context.Background(), core.Invocation{Kind: core.StepKindNode}); err == nil {
		t.Fatal("a node step without a spec is an engine fault")
	}
	if _, err := r.d.Dispatch(context.Background(), core.Invocation{Kind: core.StepKindExec}); err == nil {
		t.Fatal("an exec step without exec is an engine fault")
	}
}

// ---- side-effect ladder ----

func TestSideEffectMapping(t *testing.T) {
	for _, tc := range []struct {
		in      node.SideEffect
		want    cli.SideEffect
		dryExec bool
	}{
		{node.SideEffectNone, cli.SideEffectRead, true},
		{node.SideEffectRead, cli.SideEffectRead, true},
		{node.SideEffectWriteLocal, cli.SideEffectWriteLocal, true},
		{node.SideEffectWriteShared, cli.SideEffectWriteShared, false},
		{node.SideEffectIrreversible, cli.SideEffectDestructiveShared, false},
		{"unknown", cli.SideEffectDestructiveShared, false},
	} {
		if got := dispatch.KitSideEffect(tc.in); got != tc.want {
			t.Errorf("%s -> %s, want %s", tc.in, got, tc.want)
		}
		if got := dispatch.ExecutesInDryRun(tc.in); got != tc.dryExec {
			t.Errorf("%s executes in dry run = %v, want %v", tc.in, got, tc.dryExec)
		}
	}
}

func skipWithoutInAlias(t *testing.T) {
	t.Helper()
	if !render.InAliasAvailable() {
		t.Skip("spec/cel does not yet alias the CEL keyword `in`; {{ in.x }} does not compile")
	}
}
