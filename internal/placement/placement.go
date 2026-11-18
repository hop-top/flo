// Package placement is the local Runner: it checks where a step asks to run
// (node.md §15) and hands what phase 1 can do to the Dispatcher. A runner
// class other than local and an isolation beyond process are refused: the
// step fails with placement_refused before anything runs.
package placement

import (
	"context"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
)

// Local is the core.Runner of phase 1: every step runs in this process, as
// the profile its invocation names. Isolation `process` is what an aps
// profile already gives (env injection), so it needs nothing more here.
type Local struct {
	Dispatcher core.Dispatcher
}

// New returns a Local runner over d.
func New(d core.Dispatcher) *Local { return &Local{Dispatcher: d} }

// Of returns the placement an invocation asks for: the node's, defaulting to
// {local, none}; core steps have no spec and ask for the default.
func Of(inv core.Invocation) core.Placement {
	if inv.Node == nil {
		return core.Placement{Runner: node.RunnerLocal, Isolation: node.IsolationNone}
	}
	p := inv.Node.EffectivePlacement()
	return core.Placement{Runner: p.Runner, Isolation: p.Isolation}
}

// Check returns placement_refused when phase 1 cannot honor p, else nil.
func Check(p core.Placement, privacy node.Privacy) *core.Error {
	if p.Runner != node.RunnerLocal {
		e := core.Errorf(core.CodePlacementRefused, "runner %q is not available: steps run on the local runner", p.Runner).
			WithDetail("runner", p.Runner)
		if privacy == node.PrivacyLocal {
			e = core.Errorf(core.CodePlacementRefused,
				"runner %q is not available, and a node with privacy local cannot be placed on a remote runner", p.Runner).
				WithDetail("runner", p.Runner)
		}
		return e.WithSuggestion("use placement.runner: local")
	}
	switch p.Isolation {
	case node.IsolationNone, node.IsolationProcess:
		return nil
	}
	return core.Errorf(core.CodePlacementRefused, "isolation %q is not available: phase 1 supports none and process", p.Isolation).
		WithDetail("isolation", p.Isolation).
		WithSuggestion("use placement.isolation: none or process")
}

// Run implements core.Runner.
func (l *Local) Run(ctx context.Context, inv core.Invocation) (core.Result, error) {
	privacy := node.PrivacyCloud
	if inv.Node != nil {
		privacy = inv.Node.Privacy
	}
	if e := Check(Of(inv), privacy); e != nil {
		return core.Result{Outcome: core.OutcomeFailed, Err: e, Effect: core.EffectNone}, nil
	}
	return l.Dispatcher.Dispatch(ctx, inv) //nolint:wrapcheck // the dispatcher's own contract
}

var _ core.Runner = (*Local)(nil)
