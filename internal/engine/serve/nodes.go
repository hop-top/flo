package serve

import (
	"os"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/builtin"
	"hop.top/flo/internal/nodes/dispatch"
	"hop.top/flo/internal/nodes/plugin"
	"hop.top/flo/spec/node"
)

// Nodes are the node layers of node.md §1.2: the flow bundle's nodes/ (per
// flow), the installed packs and the plugins registered with the engine, and
// the built-ins. Registry is what the executor resolves through; Locator is
// the same layers as the spec module's resolver, for flow validation.
type Nodes struct {
	Registry *dispatch.Registry
	Pack     *dispatch.PackLayer
	Plugins  *plugin.Registry
	Builtins *builtin.Registry
	Packs    *Packs
}

func newNodes(packs *Packs, builtins *builtin.Registry) *Nodes {
	plugins := plugin.NewRegistry()
	layer := &dispatch.PackLayer{Plugins: plugins}
	for _, e := range packs.Entries() {
		layer.Add(core.NodeSource{Pack: e.Source.Pack, PackVersion: e.Source.PackVersion, Path: e.Source.Path}, e.Spec)
	}
	return &Nodes{
		Registry: dispatch.NewRegistry(dispatch.BundleLayer{}, layer, dispatch.BuiltinLayer{Builtins: builtins}),
		Pack:     layer,
		Plugins:  plugins,
		Builtins: builtins,
		Packs:    packs,
	}
}

// Locator resolves node types the way a run of a flow applied from bundle
// does: the bundle's nodes/ and trusts/ (none when bundle is empty), packs and
// plugins, built-ins. It reads the bundle from disk, so a change to a bundle
// node is seen by the next validation.
func (n *Nodes) Locator(bundle string) (*node.Locator, error) {
	var l node.Layers
	if bundle != "" {
		nodes, trusts, err := node.LoadBundle(os.DirFS(bundle))
		if err != nil {
			return nil, core.Errorf(core.CodeInvalidNode, "bundle %s: %w", bundle, err)
		}
		l.Bundle, l.BundleTrusts = nodes, trusts
	}
	l.Packs = n.Packs.Entries()
	l.PackTrusts = n.Packs.TrustEntries()
	for _, s := range n.Plugins.Specs() {
		src := node.Source{Layer: node.LayerPack}
		if t, err := s.Type(); err == nil {
			src.Pack = t.NS
		}
		l.Packs = append(l.Packs, node.Entry{Spec: s, Source: src})
	}
	for _, b := range n.Builtins.List() {
		l.Builtin = append(l.Builtin, node.Entry{Spec: b.Spec(), Source: node.Source{Layer: node.LayerBuiltin, Path: b.Spec().Path}})
	}
	loc, err := node.NewLocator(l)
	if err != nil {
		return nil, core.Wrap(core.CodeNodeConflict, err)
	}
	return loc, nil
}
