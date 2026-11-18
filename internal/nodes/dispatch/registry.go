package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/plugin"
	"hop.top/flo/spec/node"
)

// Layer is one source of node specs (node.md §1.2). A registry reads its
// layers in precedence order.
type Layer interface {
	// Specs returns every spec the layer serves for scope, each with its
	// Source.
	Specs(ctx context.Context, scope core.NodeScope) ([]core.NodeInfo, error)
}

// Registry is the core.NodeRegistry: the first layer with a match wins.
// Build it with NewRegistry in the order bundle, packs, built-ins.
type Registry struct {
	layers []Layer
}

// NewRegistry returns a registry over layers, highest precedence first.
func NewRegistry(layers ...Layer) *Registry { return &Registry{layers: layers} }

// Resolve implements core.NodeRegistry. A ref `ns/name@N` pins the major;
// `ns/name` takes the highest N of the first layer that has the node. Two
// specs of the same type in one layer (two packs) are node_conflict.
func (r *Registry) Resolve(ctx context.Context, scope core.NodeScope, ref string) (core.NodeInfo, error) {
	want, pinned, err := parseRef(ref)
	if err != nil {
		return core.NodeInfo{}, err
	}
	for _, l := range r.layers {
		infos, err := l.Specs(ctx, scope)
		if err != nil {
			return core.NodeInfo{}, err
		}
		var best []core.NodeInfo
		bestMajor := 0
		for _, in := range infos {
			t, terr := in.Spec.Type()
			if terr != nil || t.Ref() != want.Ref() || (pinned && t.Major != want.Major) {
				continue
			}
			switch {
			case t.Major > bestMajor:
				best, bestMajor = []core.NodeInfo{in}, t.Major
			case t.Major == bestMajor:
				best = append(best, in)
			}
		}
		switch len(best) {
		case 0:
			continue
		case 1:
			return best[0], nil
		}
		return core.NodeInfo{}, core.Errorf(core.CodeNodeConflict, "node %s is declared %d times in the %s layer: %s",
			best[0].Spec.Node, len(best), best[0].Source.Layer, describe(best))
	}
	return core.NodeInfo{}, core.Errorf(core.CodeNodeNotFound, "node %q not found", ref)
}

// List implements core.NodeRegistry: one entry per node type, from the
// highest layer that has it, ordered by type.
func (r *Registry) List(ctx context.Context, scope core.NodeScope) ([]core.NodeInfo, error) {
	seen := map[string]core.NodeInfo{}
	for _, l := range r.layers {
		infos, err := l.Specs(ctx, scope)
		if err != nil {
			return nil, err
		}
		for _, in := range infos {
			if _, ok := seen[in.Spec.Node]; !ok {
				seen[in.Spec.Node] = in
			}
		}
	}
	types := make([]string, 0, len(seen))
	for t := range seen {
		types = append(types, t)
	}
	sort.Strings(types)
	out := make([]core.NodeInfo, len(types))
	for i, t := range types {
		out[i] = seen[t]
	}
	return out, nil
}

func parseRef(ref string) (node.Type, bool, error) {
	if t, err := node.ParseType(ref); err == nil {
		return t, true, nil
	}
	t, err := node.ParseType(ref + "@1")
	if err != nil || strings.Contains(ref, "@") {
		return node.Type{}, false, core.Errorf(core.CodeUsage, "node reference %q: want ns/name@N or ns/name", ref)
	}
	return t, false, nil
}

func describe(infos []core.NodeInfo) string {
	var parts []string
	for _, in := range infos {
		switch {
		case in.Source.Pack != "":
			parts = append(parts, "pack "+in.Source.Pack)
		case in.Source.Path != "":
			parts = append(parts, in.Source.Path)
		default:
			parts = append(parts, string(in.Source.Layer))
		}
	}
	return strings.Join(parts, ", ")
}

// BundleLoader reads the node specs of a flow bundle (`nodes/`). The spec
// module's locator replaces FSLoader when it lands.
type BundleLoader interface {
	// Load returns the specs of the bundle directory, each with its file
	// as Source.Path. A bundle without nodes gives none.
	Load(ctx context.Context, bundle string) ([]core.NodeInfo, error)
}

// FSLoader reads `<bundle>/nodes/*.yaml|*.yml|*.json` with node.Parse.
type FSLoader struct{}

// Load implements BundleLoader.
func (FSLoader) Load(_ context.Context, bundle string) ([]core.NodeInfo, error) {
	dir := filepath.Join(bundle, "nodes")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []core.NodeInfo
	for _, e := range entries {
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".yaml", ".yml", ".json":
		default:
			continue
		}
		if e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := os.Open(path) //nolint:gosec // a node file of the flow bundle
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		spec, perr := node.Parse(f, path)
		_ = f.Close()
		if perr != nil {
			return nil, core.Errorf(core.CodeInvalidNode, "%v", perr)
		}
		spec.Path = path
		out = append(out, core.NodeInfo{Spec: spec, Source: core.NodeSource{Layer: core.LayerBundle, Path: path}})
	}
	return out, nil
}

// BundleLayer is the flow bundle's `nodes/` (highest precedence).
type BundleLayer struct{ Loader BundleLoader }

// Specs implements Layer.
func (b BundleLayer) Specs(ctx context.Context, scope core.NodeScope) ([]core.NodeInfo, error) {
	if scope.Bundle == "" {
		return nil, nil
	}
	l := b.Loader
	if l == nil {
		l = FSLoader{}
	}
	return l.Load(ctx, scope.Bundle)
}

// PackLayer holds the specs of installed packs, and of the plugins registered
// with the engine (a plugin ships its specs; its namespace is its pack).
type PackLayer struct {
	mu      sync.RWMutex
	infos   []core.NodeInfo
	Plugins *plugin.Registry
}

// Add registers the specs of a pack as served by source (Layer pack).
func (p *PackLayer) Add(source core.NodeSource, specs ...*node.Spec) {
	p.mu.Lock()
	defer p.mu.Unlock()
	source.Layer = core.LayerPack
	for _, s := range specs {
		p.infos = append(p.infos, core.NodeInfo{Spec: s, Source: source})
	}
}

// Specs implements Layer.
func (p *PackLayer) Specs(context.Context, core.NodeScope) ([]core.NodeInfo, error) {
	p.mu.RLock()
	out := append([]core.NodeInfo(nil), p.infos...)
	p.mu.RUnlock()
	if p.Plugins != nil {
		for _, s := range p.Plugins.Specs() {
			ns := ""
			if t, err := s.Type(); err == nil {
				ns = t.NS
			}
			out = append(out, core.NodeInfo{Spec: s, Source: core.NodeSource{Layer: core.LayerPack, Pack: ns}})
		}
	}
	return out, nil
}

// BuiltinLayer serves the specs of the built-in registry (lowest precedence).
type BuiltinLayer struct{ Builtins core.BuiltinRegistry }

// Specs implements Layer.
func (b BuiltinLayer) Specs(context.Context, core.NodeScope) ([]core.NodeInfo, error) {
	var out []core.NodeInfo
	for _, n := range b.Builtins.List() {
		out = append(out, core.NodeInfo{Spec: n.Spec(), Source: core.NodeSource{Layer: core.LayerBuiltin}})
	}
	return out, nil
}

var _ core.NodeRegistry = (*Registry)(nil)
