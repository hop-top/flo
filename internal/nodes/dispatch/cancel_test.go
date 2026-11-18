package dispatch_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hop.top/kit/go/ai/ext"
	"hop.top/kit/go/runtime/sideeffect/real"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/command"
	"hop.top/flo/internal/nodes/dispatch"
	httpnode "hop.top/flo/internal/nodes/http"
	"hop.top/flo/internal/nodes/plugin"
	"hop.top/flo/internal/placement"
	"hop.top/flo/spec/node"
)

// cancelAfter cancels ctx 150ms into the call and reports how long Run took.
func cancelRun(t *testing.T, run func(ctx context.Context) (core.Result, error)) (core.Result, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(150*time.Millisecond, cancel)
	start := time.Now()
	res, err := run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return res, time.Since(start)
}

func assertCancelled(t *testing.T, res core.Result, d time.Duration) {
	t.Helper()
	if d > time.Second {
		t.Fatalf("returned after %s, want ~150ms", d)
	}
	if res.Outcome != core.OutcomeFailed || res.Err == nil || res.Err.Code != core.CodeRunCancelled || res.Effect != core.EffectUnknown {
		t.Fatalf("res = %+v", res)
	}
}

func placed(d core.Dispatcher) core.Runner { return placement.New(d) }

func TestCancelCommandKillsGroupKeepsPartial(t *testing.T) {
	d := dispatch.New(dispatch.Deps{Commands: &command.Runner{Exec: real.Exec{}, Aps: "/nonexistent/aps-never-run"}})
	s := spec(t, "node: x/slow@1\nkind: command\nargv: [sh, -c, 'echo partial; sleep 30; echo done']\ninputs: {}\noutputs: { schema: {} }\ncapabilities: []\nside_effect: read\nprivacy: local\n")
	i := inv(s, nil)
	i.RunAs = ""
	res, took := cancelRun(t, func(ctx context.Context) (core.Result, error) { return placed(d).Run(ctx, i) })
	assertCancelled(t, res, took)
	if res.Raw.(map[string]any)["stdout"] != "partial\n" {
		t.Fatalf("partial output kept: %v", res.Raw)
	}
}

func TestCancelCoreExec(t *testing.T) {
	d := dispatch.New(dispatch.Deps{Commands: &command.Runner{Exec: real.Exec{}}})
	i := core.Invocation{Kind: core.StepKindExec, Exec: &core.ExecInput{Argv: []string{"sh", "-c", "echo partial; sleep 30; echo done"}}}
	res, took := cancelRun(t, func(ctx context.Context) (core.Result, error) { return placed(d).Run(ctx, i) })
	assertCancelled(t, res, took)
}

func TestCancelHTTP(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	d := dispatch.New(dispatch.Deps{HTTP: real.NewHTTP(httpnode.NewClient())})
	s := spec(t, "node: x/h@1\nkind: http\nrequest: { method: POST, url: '"+srv.URL+"', json: {} }\ninputs: {}\noutputs: { schema: {} }\ncapabilities: []\nside_effect: write-local\nprivacy: local\n")
	res, took := cancelRun(t, func(ctx context.Context) (core.Result, error) { return placed(d).Run(ctx, inv(s, nil)) })
	assertCancelled(t, res, took)
}

type blockingPlugin struct{ specs []*node.Spec }

func (p *blockingPlugin) Meta() ext.Metadata           { return ext.Metadata{Name: "blk"} }
func (p *blockingPlugin) Capabilities() ext.Capability { return ext.CapRegistry }
func (p *blockingPlugin) Init(context.Context) error   { return nil }
func (p *blockingPlugin) Close() error                 { return nil }
func (p *blockingPlugin) Specs() []*node.Spec          { return p.specs }
func (p *blockingPlugin) Call(context.Context, plugin.Call) (plugin.Reply, error) {
	time.Sleep(3 * time.Second) // ignores its context: the executor must not wait
	return plugin.Reply{}, nil
}

func TestCancelPluginThatIgnoresContext(t *testing.T) {
	reg := plugin.NewRegistry()
	s := spec(t, pluginSpec)
	if err := reg.RegisterNode(&blockingPlugin{specs: []*node.Spec{s}}); err != nil {
		t.Fatal(err)
	}
	d := dispatch.New(dispatch.Deps{Plugins: reg})
	res, took := cancelRun(t, func(ctx context.Context) (core.Result, error) {
		return placed(d).Run(ctx, inv(s, map[string]any{"calendar": "w"}))
	})
	assertCancelled(t, res, took)
}

type blockingBuiltin struct{ s *node.Spec }

func (b blockingBuiltin) Spec() *node.Spec { return b.s }
func (b blockingBuiltin) Execute(context.Context, core.Invocation) (core.Result, error) {
	time.Sleep(3 * time.Second)
	return core.Result{Outcome: core.OutcomeOK}, nil
}

func TestCancelBuiltin(t *testing.T) {
	r := newRig(t)
	s := spec(t, "node: flo/slowtest@1\nkind: builtin\ninputs: {}\noutputs: { schema: {} }\ncapabilities: []\nside_effect: none\nprivacy: local\n")
	bi := newBuiltins(t, blockingBuiltin{s})
	d := dispatch.New(dispatch.Deps{Builtins: bi})
	_ = r
	res, took := cancelRun(t, func(ctx context.Context) (core.Result, error) { return placed(d).Run(ctx, inv(s, nil)) })
	assertCancelled(t, res, took)
}
