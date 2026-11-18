// Package plugin holds the plugin interfaces of node.md §7 and the registry
// the dispatcher runs them from. A plugin is code behind kit's extension
// interface, loaded in process in phase 1; it ships node specs next to its
// code, and the specs are the contract: inputs are validated before the call
// and outputs after. A plugin sees only `in`, `config` and the run's
// identity (never state, other steps' results or the run tree).
package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"hop.top/kit/go/ai/ext"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
)

// Call is one invocation of a plugin node.
type Call struct {
	// Node is the node type `ns/name@N` being called (a plugin may serve
	// several).
	Node   string
	Tenant core.TenantID
	// Run is the run's identity; StepID and Index say which step (and
	// for_each element) the call is for.
	Run     core.RunContext
	StepID  string
	Index   *int
	Attempt int
	// In is the validated input, defaults applied. Config is the pack
	// configuration (`config.<pack>.*`).
	In     map[string]any
	Config map[string]any
	// RunAs is the profile the step acts as.
	RunAs string
	// IdempotencyKey is the key the target system recognizes a repeat by.
	IdempotencyKey string
}

// Reply is what a plugin node returns: the value `results.<step>` reads,
// matching the node's outputs.schema, and what the call cost.
type Reply struct {
	Output any
	Cost   core.Cost
	// Calls is how many provider calls were made, for the budget; 0 is 1.
	Calls int
}

// NodePlugin serves one or more step nodes.
type NodePlugin interface {
	ext.Extension
	// Specs returns the node specs it serves (kind plugin, role step).
	Specs() []*node.Spec
	// Call runs one invocation. A failure is an error: a *Failed to say
	// whether the effect happened, a *core.Error for a classified failure,
	// anything else is an unknown-effect run_failed.
	Call(ctx context.Context, c Call) (Reply, error)
}

// TriggerRequest asks a trigger plugin what exists now.
type TriggerRequest struct {
	Tenant core.TenantID
	// Node is the trigger node type.
	Node string
	// Options are the `on:` entry's options (the node's inputs).
	Options map[string]any
	Config  map[string]any
	// Cursor is the sync token of the last poll; empty on the first.
	Cursor string
	// From and To bound the window the engine is planning (zero for none).
	From, To time.Time
}

// Event is one thing a trigger reports: a stable id (the run key's source,
// flow.md §6.1) and its payload (outputs.schema of the trigger node).
type Event struct {
	ID      string
	Payload map[string]any
}

// TriggerBatch is a poll's answer: what exists, and the token to poll from
// next. The engine owns timing; the plugin never says when to fire.
type TriggerBatch struct {
	Events []Event
	Cursor string
}

// TriggerPlugin serves one or more trigger nodes (role trigger).
type TriggerPlugin interface {
	ext.Extension
	// Specs returns the trigger node specs it serves.
	Specs() []*node.Spec
	// Poll reports the events that exist and what changed since Cursor.
	Poll(ctx context.Context, r TriggerRequest) (TriggerBatch, error)
}

// Failed is a plugin's failure with the effect it is known to have had.
type Failed struct {
	Err    *core.Error
	Effect core.Effect
}

// Error implements error.
func (f *Failed) Error() string { return f.Err.Error() }

// Unwrap returns the classified error.
func (f *Failed) Unwrap() error { return f.Err }

// Fail returns a *Failed.
func Fail(err *core.Error, effect core.Effect) error { return &Failed{Err: err, Effect: effect} }

// Classify turns a plugin's error into the failure and effect of a Result.
func Classify(err error) (*core.Error, core.Effect) {
	var f *Failed
	if errors.As(err, &f) {
		return f.Err, f.Effect
	}
	var ce *core.Error
	if errors.As(err, &ce) {
		return ce, core.EffectUnknown
	}
	return core.Errorf(core.CodeRunFailed, "plugin failed: %v", err), core.EffectUnknown
}

// Registry holds the plugins by the node types they serve.
type Registry struct {
	mu       sync.RWMutex
	nodes    map[string]NodePlugin
	triggers map[string]TriggerPlugin
	all      []ext.Extension
	inited   map[ext.Extension]bool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{nodes: map[string]NodePlugin{}, triggers: map[string]TriggerPlugin{}, inited: map[ext.Extension]bool{}}
}

// RegisterNode adds a node plugin under each spec it serves. A node type
// already served is node_conflict; a spec that is not a plugin step node is
// invalid_node.
func (r *Registry) RegisterNode(p NodePlugin) error {
	return r.register(p, p.Specs(), node.RoleStep, func(ref string) bool {
		_, ok := r.nodes[ref]
		return ok
	}, func(ref string) { r.nodes[ref] = p })
}

// RegisterTrigger adds a trigger plugin under each spec it serves.
func (r *Registry) RegisterTrigger(p TriggerPlugin) error {
	return r.register(p, p.Specs(), node.RoleTrigger, func(ref string) bool {
		_, ok := r.triggers[ref]
		return ok
	}, func(ref string) { r.triggers[ref] = p })
}

func (r *Registry) register(p ext.Extension, specs []*node.Spec, role node.Role, taken func(string) bool, set func(string)) error {
	if len(specs) == 0 {
		return core.Errorf(core.CodeInvalidNode, "plugin %q serves no node", p.Meta().Name)
	}
	for _, s := range specs {
		if s.Kind != node.KindPlugin || s.EffectiveRole() != role {
			return core.Errorf(core.CodeInvalidNode, "plugin %q: %s must be kind plugin, role %s", p.Meta().Name, s.Node, role)
		}
		if _, err := s.Type(); err != nil {
			return core.Errorf(core.CodeInvalidNode, "plugin %q: %v", p.Meta().Name, err)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range specs {
		if taken(s.Node) {
			return core.Errorf(core.CodeNodeConflict, "node %s is served by two plugins", s.Node)
		}
	}
	for _, s := range specs {
		set(s.Node)
	}
	r.all = append(r.all, p)
	return nil
}

// Node returns the plugin serving a node type.
func (r *Registry) Node(ref string) (NodePlugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.nodes[ref]
	return p, ok
}

// Trigger returns the plugin serving a trigger node type.
func (r *Registry) Trigger(ref string) (TriggerPlugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.triggers[ref]
	return p, ok
}

// Specs returns every spec the registered plugins serve, by node type.
func (r *Registry) Specs() []*node.Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*node.Spec
	seen := map[ext.Extension]bool{}
	for _, p := range r.all {
		if seen[p] {
			continue
		}
		seen[p] = true
		switch t := p.(type) {
		case NodePlugin:
			out = append(out, t.Specs()...)
		case TriggerPlugin:
			out = append(out, t.Specs()...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

// Init activates every registered plugin not yet activated (ext.Extension).
func (r *Registry) Init(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.all {
		if r.inited[p] {
			continue
		}
		if err := p.Init(ctx); err != nil {
			return fmt.Errorf("init plugin %q: %w", p.Meta().Name, err)
		}
		r.inited[p] = true
	}
	return nil
}

// Close deactivates the activated plugins, reporting the first failure.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var first error
	for _, p := range r.all {
		if !r.inited[p] {
			continue
		}
		if err := p.Close(); err != nil && first == nil {
			first = fmt.Errorf("close plugin %q: %w", p.Meta().Name, err)
		}
		delete(r.inited, p)
	}
	return first
}

// Executor runs plugin nodes for the dispatcher.
type Executor struct{ Plugins *Registry }

// Plan returns what a dry run shows: the input the plugin would get.
func (x *Executor) Plan(inv core.Invocation) map[string]any {
	return map[string]any{"plugin": inv.Node.Node, "in": inv.In}
}

// Execute calls the plugin once. A failure of the step is a Result with
// OutcomeFailed.
func (x *Executor) Execute(ctx context.Context, inv core.Invocation) (core.Result, error) {
	p, ok := x.Plugins.Node(inv.Node.Node)
	if !ok {
		return core.Result{}, core.Errorf(core.CodeNodeNotFound, "no plugin serves %s", inv.Node.Node)
	}
	rep, err := p.Call(ctx, Call{
		Node: inv.Node.Node, Tenant: inv.Tenant, Run: inv.Run, StepID: inv.StepID, Index: inv.Index, Attempt: inv.Attempt,
		In: inv.In, Config: inv.Config, RunAs: inv.RunAs, IdempotencyKey: inv.IdempotencyKey,
	})
	if err != nil {
		e, eff := Classify(err)
		return core.Result{Outcome: core.OutcomeFailed, Err: e, Effect: eff, Calls: 1}, nil
	}
	return core.Result{Outcome: core.OutcomeOK, Output: rep.Output, Cost: rep.Cost, Calls: rep.Calls, Effect: core.EffectDone}, nil
}
