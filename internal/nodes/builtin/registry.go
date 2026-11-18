// Package builtin holds the engine's built-in nodes (node.md §6): their
// specs are embedded, their code runs in the engine. The registry serves the
// dispatcher and the node registry.
package builtin

import (
	"embed"
	"fmt"
	"sort"
	"sync"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/node"
)

//go:embed specs/*.yaml
var specFS embed.FS

// Spec returns the embedded spec of a built-in by node type, for the nodes
// of this package.
func Spec(ref string) (*node.Spec, error) {
	name := fmt.Sprintf("specs/%s.yaml", fileOf(ref))
	data, err := specFS.ReadFile(name)
	if err != nil {
		return nil, core.Errorf(core.CodeNodeNotFound, "no embedded spec for %s", ref)
	}
	s, err := node.Decode(data, name)
	if err != nil {
		return nil, fmt.Errorf("embedded spec %s: %w", name, err)
	}
	s.Path = "builtin/" + name
	return s, nil
}

// fileOf maps `ns/name@N` to the file name `ns_name_N`.
func fileOf(ref string) string {
	t, err := node.ParseType(ref)
	if err != nil {
		return ref
	}
	return fmt.Sprintf("%s_%s_%d", t.NS, t.Name, t.Major)
}

// Registry is the core.BuiltinRegistry.
type Registry struct {
	mu    sync.RWMutex
	nodes map[string]core.BuiltinNode
}

// NewRegistry returns an empty registry; RegisterStd fills it.
func NewRegistry() *Registry { return &Registry{nodes: map[string]core.BuiltinNode{}} }

// Register implements core.BuiltinRegistry. A second registration of a node
// type is node_conflict.
func (r *Registry) Register(n core.BuiltinNode) error {
	s := n.Spec()
	if s == nil || s.Kind != node.KindBuiltin {
		return core.Errorf(core.CodeInvalidNode, "a built-in needs a spec of kind builtin")
	}
	if _, err := s.Type(); err != nil {
		return core.Errorf(core.CodeInvalidNode, "%v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.nodes[s.Node]; ok {
		return core.Errorf(core.CodeNodeConflict, "built-in %s is already registered", s.Node)
	}
	r.nodes[s.Node] = n
	return nil
}

// Lookup implements core.BuiltinRegistry.
func (r *Registry) Lookup(ref string) (core.BuiltinNode, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.nodes[ref]
	return n, ok
}

// List implements core.BuiltinRegistry.
func (r *Registry) List() []core.BuiltinNode {
	r.mu.RLock()
	defer r.mu.RUnlock()
	refs := make([]string, 0, len(r.nodes))
	for ref := range r.nodes {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	out := make([]core.BuiltinNode, len(refs))
	for i, ref := range refs {
		out[i] = r.nodes[ref]
	}
	return out
}

// Deps are what the standard built-ins need.
type Deps struct {
	Outputs core.OutputStore
	IDs     core.IDs
	Clock   core.Clock
}

// RegisterStd registers flo/dedupe@1, flo/filter@1 and flo/output@1.
// flo/approval@1 and the AI nodes are registered by their own packages.
func RegisterStd(r core.BuiltinRegistry, d Deps) error {
	for _, mk := range []func() (core.BuiltinNode, error){
		newDedupe, newFilter, func() (core.BuiltinNode, error) { return newOutput(d) },
	} {
		n, err := mk()
		if err != nil {
			return err
		}
		if err := r.Register(n); err != nil {
			return err //nolint:wrapcheck // already a classified *core.Error
		}
	}
	return nil
}

var _ core.BuiltinRegistry = (*Registry)(nil)
