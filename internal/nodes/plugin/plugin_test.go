package plugin_test

import (
	"context"
	"errors"
	"testing"

	"hop.top/kit/go/ai/ext"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/plugin"
	"hop.top/flo/spec/node"
)

func spec(t *testing.T, ref string, role string) *node.Spec {
	t.Helper()
	s, err := node.Decode([]byte("node: "+ref+"\nkind: plugin\nrole: "+role+"\ninputs: {}\noutputs: { schema: {} }\ncapabilities: []\nside_effect: read\nprivacy: local\n"), "n.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type fake struct {
	specs          []*node.Spec
	inited, closed int
	call           func(plugin.Call) (plugin.Reply, error)
}

func (f *fake) Meta() ext.Metadata           { return ext.Metadata{Name: "fake", Version: "0.0.1"} }
func (f *fake) Capabilities() ext.Capability { return ext.CapRegistry }
func (f *fake) Init(context.Context) error   { f.inited++; return nil }
func (f *fake) Close() error                 { f.closed++; return nil }
func (f *fake) Specs() []*node.Spec          { return f.specs }
func (f *fake) Call(_ context.Context, c plugin.Call) (plugin.Reply, error) {
	return f.call(c)
}

type fakeTrigger struct{ fake }

func (f *fakeTrigger) Poll(context.Context, plugin.TriggerRequest) (plugin.TriggerBatch, error) {
	return plugin.TriggerBatch{Events: []plugin.Event{{ID: "e1"}}, Cursor: "c1"}, nil
}

func TestRegistry(t *testing.T) {
	r := plugin.NewRegistry()
	a := &fake{specs: []*node.Spec{spec(t, "x/a@1", "step"), spec(t, "x/b@1", "step")}}
	if err := r.RegisterNode(a); err != nil {
		t.Fatal(err)
	}
	if p, ok := r.Node("x/b@1"); !ok || p != plugin.NodePlugin(a) {
		t.Fatal("lookup by node type")
	}
	if err := r.RegisterNode(&fake{specs: []*node.Spec{spec(t, "x/a@1", "step")}}); !core.IsCode(err, core.CodeNodeConflict) {
		t.Fatalf("two plugins, one node: %v", err)
	}
	if err := r.RegisterNode(&fake{specs: []*node.Spec{spec(t, "x/t@1", "trigger")}}); !core.IsCode(err, core.CodeInvalidNode) {
		t.Fatalf("a trigger spec is not a step plugin: %v", err)
	}
	tr := &fakeTrigger{fake{specs: []*node.Spec{spec(t, "cal/events@1", "trigger")}}}
	if err := r.RegisterTrigger(tr); err != nil {
		t.Fatal(err)
	}
	if got := len(r.Specs()); got != 3 {
		t.Fatalf("specs = %d", got)
	}
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = r.Init(context.Background())
	if a.inited != 1 || tr.inited != 1 {
		t.Fatalf("each plugin is activated once: %d %d", a.inited, tr.inited)
	}
	_ = r.Close()
	if a.closed != 1 {
		t.Fatalf("closed = %d", a.closed)
	}
}

func TestExecutorClassifiesFailures(t *testing.T) {
	r := plugin.NewRegistry()
	f := &fake{specs: []*node.Spec{spec(t, "x/a@1", "step")}}
	_ = r.RegisterNode(f)
	x := &plugin.Executor{Plugins: r}
	inv := core.Invocation{Node: f.specs[0], In: map[string]any{"k": 1}, Attempt: 2, StepID: "s"}

	f.call = func(c plugin.Call) (plugin.Reply, error) {
		if c.In["k"] != 1 || c.Attempt != 2 || c.StepID != "s" {
			t.Errorf("call = %+v", c)
		}
		return plugin.Reply{Output: "v", Cost: core.Cost{USD: 1}}, nil
	}
	res, _ := x.Execute(context.Background(), inv)
	if res.Outcome != core.OutcomeOK || res.Output != "v" || res.Cost.USD != 1 {
		t.Fatalf("res = %+v", res)
	}
	for name, tc := range map[string]struct {
		err    error
		code   core.Code
		effect core.Effect
	}{
		"plain":      {errors.New("boom"), core.CodeRunFailed, core.EffectUnknown},
		"classified": {core.New(core.CodeInputInvalid, "x"), core.CodeInputInvalid, core.EffectUnknown},
		"no effect":  {plugin.Fail(core.New(core.CodeRunFailed, "x"), core.EffectNone), core.CodeRunFailed, core.EffectNone},
	} {
		f.call = func(plugin.Call) (plugin.Reply, error) { return plugin.Reply{}, tc.err }
		res, _ = x.Execute(context.Background(), inv)
		if res.Outcome != core.OutcomeFailed || res.Err.Code != tc.code || res.Effect != tc.effect {
			t.Fatalf("%s: res = %+v", name, res)
		}
	}
	if _, err := x.Execute(context.Background(), core.Invocation{Node: spec(t, "x/zz@1", "step")}); !core.IsCode(err, core.CodeNodeNotFound) {
		t.Fatalf("err = %v", err)
	}
}
