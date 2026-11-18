package placement_test

import (
	"context"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/internal/placement"
	"hop.top/flo/spec/node"
)

type counting struct{ n int }

func (c *counting) Dispatch(context.Context, core.Invocation) (core.Result, error) {
	c.n++
	return core.Result{Outcome: core.OutcomeOK}, nil
}

func nodeWith(t *testing.T, placementYAML, privacy string) *node.Spec {
	t.Helper()
	s, err := node.Decode([]byte("node: x/a@1\nkind: command\nargv: [x]\ninputs: {}\noutputs: { schema: {} }\ncapabilities: []\nside_effect: read\nprivacy: "+privacy+"\n"+placementYAML), "n.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLocalRunnerPlacements(t *testing.T) {
	for name, tc := range map[string]struct {
		yaml, privacy string
		refused       bool
	}{
		"default":           {"", "local", false},
		"local none":        {"placement: { runner: local, isolation: none }", "local", false},
		"local process":     {"placement: { runner: local, isolation: process }", "cloud", false},
		"remote runner":     {"placement: { runner: m3, isolation: none }", "cloud", true},
		"remote local-priv": {"placement: { runner: m3 }", "local", true},
		"container":         {"placement: { runner: local, isolation: container }", "local", true},
		"pod":               {"placement: { isolation: pod }", "local", true},
		"unknown isolation": {"placement: { isolation: vm }", "local", true},
	} {
		t.Run(name, func(t *testing.T) {
			d := &counting{}
			res, err := placement.New(d).Run(context.Background(), core.Invocation{Kind: core.StepKindNode, Node: nodeWith(t, tc.yaml, tc.privacy)})
			if err != nil {
				t.Fatal(err)
			}
			if tc.refused {
				if res.Outcome != core.OutcomeFailed || res.Err.Code != core.CodePlacementRefused || res.Effect != core.EffectNone || d.n != 0 {
					t.Fatalf("a refused placement runs nothing: res=%+v dispatched=%d", res, d.n)
				}
				return
			}
			if res.Outcome != core.OutcomeOK || d.n != 1 {
				t.Fatalf("res=%+v dispatched=%d", res, d.n)
			}
		})
	}
}

func TestCoreStepsRunLocally(t *testing.T) {
	d := &counting{}
	for _, k := range []core.StepKind{core.StepKindExec, core.StepKindHuman} {
		if res, _ := placement.New(d).Run(context.Background(), core.Invocation{Kind: k}); res.Outcome != core.OutcomeOK {
			t.Fatalf("%s: %+v", k, res)
		}
	}
	if d.n != 2 {
		t.Fatalf("dispatched = %d", d.n)
	}
}

func TestRunnerPassesDispatcherResultThrough(t *testing.T) {
	fx := coretest.New()
	fx.Dispatcher.Handle(func(core.Invocation) bool { return true }, func(core.Invocation) (core.Result, error) {
		return core.Result{}, core.Errorf(core.CodeInternal, "fault")
	})
	if _, err := placement.New(fx.Dispatcher).Run(context.Background(), core.Invocation{Kind: core.StepKindExec}); !core.IsCode(err, core.CodeInternal) {
		t.Fatalf("an engine fault stays an error: %v", err)
	}
}
