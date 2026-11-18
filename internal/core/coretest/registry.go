package coretest

import (
	"context"
	"sort"
	"sync"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
)

// Nodes is the fake core.NodeRegistry. Add registers specs per layer;
// Resolve applies the precedence of node.md §1.2 (bundle, pack, built-in) and,
// within a layer, the highest major unless the ref pins one.
type Nodes struct {
	mu      sync.Mutex
	entries []core.NodeInfo
}

var layerRank = map[core.NodeLayer]int{core.LayerBundle: 3, core.LayerPack: 2, core.LayerBuiltin: 1}

// Add registers a spec as served by source.
func (n *Nodes) Add(spec *node.Spec, source core.NodeSource) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.entries = append(n.entries, core.NodeInfo{Spec: spec, Source: source})
}

// AddBuiltin registers a spec as a built-in.
func (n *Nodes) AddBuiltin(spec *node.Spec) {
	n.Add(spec, core.NodeSource{Layer: core.LayerBuiltin})
}

// Resolve implements core.NodeRegistry.
func (n *Nodes) Resolve(_ context.Context, _ core.NodeScope, ref string) (core.NodeInfo, error) {
	want, pinned, err := parseRef(ref)
	if err != nil {
		return core.NodeInfo{}, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	var best []core.NodeInfo
	for _, e := range n.entries {
		t, err := e.Spec.Type()
		if err != nil || t.Ref() != want.Ref() || (pinned && t.Major != want.Major) {
			continue
		}
		if len(best) == 0 {
			best = []core.NodeInfo{e}
			continue
		}
		bt, _ := best[0].Spec.Type()
		switch {
		case layerRank[e.Source.Layer] > layerRank[best[0].Source.Layer]:
			best = []core.NodeInfo{e}
		case layerRank[e.Source.Layer] == layerRank[best[0].Source.Layer] && t.Major > bt.Major:
			best = []core.NodeInfo{e}
		case layerRank[e.Source.Layer] == layerRank[best[0].Source.Layer] && t.Major == bt.Major:
			best = append(best, e)
		}
	}
	switch {
	case len(best) == 0:
		return core.NodeInfo{}, core.Errorf(core.CodeNodeNotFound, "node %q not found", ref)
	case len(best) > 1 && best[0].Source.Layer == core.LayerPack:
		return core.NodeInfo{}, core.Errorf(core.CodeNodeConflict, "node %q is declared by %d packs", ref, len(best))
	}
	return best[0], nil
}

// parseRef accepts `ns/name@N` (pinned) and `ns/name`.
func parseRef(ref string) (node.Type, bool, error) {
	if t, err := node.ParseType(ref); err == nil {
		return t, true, nil
	}
	t, err := node.ParseType(ref + "@1")
	if err != nil {
		return node.Type{}, false, core.Errorf(core.CodeUsage, "node reference %q: want ns/name@N", ref)
	}
	return t, false, nil
}

// List implements core.NodeRegistry: one entry per node type, the winner of
// Resolve.
func (n *Nodes) List(ctx context.Context, scope core.NodeScope) ([]core.NodeInfo, error) {
	n.mu.Lock()
	seen := map[string]bool{}
	var types []string
	for _, e := range n.entries {
		if t, err := e.Spec.Type(); err == nil && !seen[t.String()] {
			seen[t.String()] = true
			types = append(types, t.String())
		}
	}
	n.mu.Unlock()
	sort.Strings(types)
	var out []core.NodeInfo
	for _, t := range types {
		info, err := n.Resolve(ctx, scope, t)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

// Builtins is the fake core.BuiltinRegistry.
type Builtins struct {
	mu    sync.Mutex
	nodes map[string]core.BuiltinNode
}

// Register implements core.BuiltinRegistry.
func (b *Builtins) Register(n core.BuiltinNode) error {
	spec := n.Spec()
	if spec == nil {
		return core.Errorf(core.CodeInvalidNode, "a built-in needs a spec")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.nodes == nil {
		b.nodes = map[string]core.BuiltinNode{}
	}
	if _, ok := b.nodes[spec.Node]; ok {
		return core.Errorf(core.CodeNodeConflict, "built-in %s is already registered", spec.Node)
	}
	b.nodes[spec.Node] = n
	return nil
}

// Lookup implements core.BuiltinRegistry.
func (b *Builtins) Lookup(ref string) (core.BuiltinNode, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.nodes[ref]
	return n, ok
}

// List implements core.BuiltinRegistry.
func (b *Builtins) List() []core.BuiltinNode {
	b.mu.Lock()
	defer b.mu.Unlock()
	refs := make([]string, 0, len(b.nodes))
	for r := range b.nodes {
		refs = append(refs, r)
	}
	sort.Strings(refs)
	out := make([]core.BuiltinNode, len(refs))
	for i, r := range refs {
		out[i] = b.nodes[r]
	}
	return out
}

// FuncNode adapts a spec and a function to core.BuiltinNode.
type FuncNode struct {
	S  *node.Spec
	Fn func(context.Context, core.Invocation) (core.Result, error)
}

// Spec implements core.BuiltinNode.
func (f FuncNode) Spec() *node.Spec { return f.S }

// Execute implements core.BuiltinNode.
func (f FuncNode) Execute(ctx context.Context, inv core.Invocation) (core.Result, error) {
	return f.Fn(ctx, inv)
}
