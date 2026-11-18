package coretest

import (
	"context"
	"sync"

	"hop.top/flo/internal/core"
)

// Script is the engine behind the Dispatcher and Runner fakes: it answers
// invocations from rules a test registers, and records every call. A call
// matches the first rule that accepts it; with no rule the answer is a
// successful result with a nil output.
//
// Script is concurrency-safe and tracks in-flight calls, so a test of
// for_each concurrency can assert MaxInFlight.
type Script struct {
	mu       sync.Mutex
	rules    []*rule
	calls    []core.Invocation
	inFlight int
	maxIn    int
	// Default answers calls no rule matches. Nil means ok with a nil output.
	Default func(core.Invocation) (core.Result, error)
}

type rule struct {
	match func(core.Invocation) bool
	fn    func(core.Invocation) (core.Result, error)
}

// OK is a successful Result with the given output.
func OK(output any) core.Result {
	return core.Result{Outcome: core.OutcomeOK, Output: output, Effect: core.EffectDone}
}

// Fail is a failed Result with the given code and message. Whether the
// failure is retried follows the code's transience; effect says whether the
// world may have changed.
func Fail(code core.Code, msg string, effect core.Effect) core.Result {
	return core.Result{Outcome: core.OutcomeFailed, Err: core.New(code, msg), Effect: effect}
}

// Park is a Result that parks the step on an approval request.
func Park(req core.ApprovalRequest) core.Result {
	return core.Result{Outcome: core.OutcomePark, Park: &req, Effect: core.EffectNone}
}

// Handle adds a rule: invocations accepted by match are answered by fn.
func (s *Script) Handle(match func(core.Invocation) bool, fn func(core.Invocation) (core.Result, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, &rule{match: match, fn: fn})
}

// ForStep answers the calls of one step id with results in order: the first
// call gets results[0], the second results[1], and so on; the last result
// repeats. Calls for every element of a for_each step share the sequence.
func (s *Script) ForStep(stepID string, results ...core.Result) {
	var mu sync.Mutex
	n := 0
	s.Handle(func(inv core.Invocation) bool { return inv.StepID == stepID }, func(core.Invocation) (core.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		r := results[min(n, len(results)-1)]
		n++
		return r, nil
	})
}

// ForNode answers every call to a node type.
func (s *Script) ForNode(nodeType string, fn func(core.Invocation) (core.Result, error)) {
	s.Handle(func(inv core.Invocation) bool {
		return inv.Node != nil && inv.Node.Node == nodeType
	}, fn)
}

// do answers one invocation and records it.
func (s *Script) do(_ context.Context, inv core.Invocation) (core.Result, error) {
	s.mu.Lock()
	s.calls = append(s.calls, clone(inv))
	s.inFlight++
	if s.inFlight > s.maxIn {
		s.maxIn = s.inFlight
	}
	var fn func(core.Invocation) (core.Result, error)
	for _, r := range s.rules {
		if r.match(inv) {
			fn = r.fn
			break
		}
	}
	def := s.Default
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()
	switch {
	case fn != nil:
		return fn(inv)
	case def != nil:
		return def(inv)
	}
	return OK(nil), nil
}

// Calls returns the invocations received, in arrival order.
func (s *Script) Calls() []core.Invocation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.calls)
}

// CallsFor returns the invocations of one step id.
func (s *Script) CallsFor(stepID string) []core.Invocation {
	var out []core.Invocation
	for _, c := range s.Calls() {
		if c.StepID == stepID {
			out = append(out, c)
		}
	}
	return out
}

// MaxInFlight returns the most invocations ever running at once.
func (s *Script) MaxInFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxIn
}

// Dispatcher is the fake core.Dispatcher.
type Dispatcher struct{ Script }

// Dispatch implements core.Dispatcher.
func (d *Dispatcher) Dispatch(ctx context.Context, inv core.Invocation) (core.Result, error) {
	return d.do(ctx, inv)
}

// Runner is the fake core.Runner, the seam the executor is tested against.
type Runner struct{ Script }

// Run implements core.Runner.
func (r *Runner) Run(ctx context.Context, inv core.Invocation) (core.Result, error) {
	return r.do(ctx, inv)
}

// Dispatcher returns a Dispatcher answering from this script.
func (s *Script) Dispatcher() core.Dispatcher { return scriptDispatcher{s} }

type scriptDispatcher struct{ s *Script }

func (d scriptDispatcher) Dispatch(ctx context.Context, inv core.Invocation) (core.Result, error) {
	return d.s.do(ctx, inv)
}
