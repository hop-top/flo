package node

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Layer is where a node spec was found (node.md §1.2): the flow bundle's
// nodes/, an installed pack, or the engine's built-ins, in that order of
// precedence.
type Layer string

// Resolution layers, highest precedence first.
const (
	LayerBundle  Layer = "bundle"
	LayerPack    Layer = "pack"
	LayerBuiltin Layer = "builtin"
)

// Source says which document served a type: the layer and, for a pack, the
// exact pack version (the step record names it; node.md §2).
type Source struct {
	Layer       Layer
	Pack        string
	PackVersion string
	Path        string
}

func (s Source) String() string {
	if s.Layer == LayerPack {
		return fmt.Sprintf("pack %s@%s (%s)", s.Pack, s.PackVersion, s.Path)
	}
	return fmt.Sprintf("%s (%s)", s.Layer, s.Path)
}

// Entry is a node spec and where it came from.
type Entry struct {
	Spec   *Spec
	Source Source
}

// TrustEntry is a trust provider and where it came from.
type TrustEntry struct {
	Provider *TrustProvider
	Source   Source
}

// Layers are the documents available to resolve against, per layer. The
// Packs layers hold every installed pack together: two packs may not
// declare the same type.
type Layers struct {
	Bundle       []Entry
	Packs        []Entry
	Builtin      []Entry
	BundleTrusts []TrustEntry
	PackTrusts   []TrustEntry
}

// Locator resolves node types and trust providers over the layers.
// Precedence is bundle, then packs, then built-ins; an unpinned reference
// takes the highest major of the first layer that has the name, a pinned
// one the first layer that has that exact major (flow locator rules,
// node.md §1.2).
type Locator struct {
	nodes  [3][]Entry // indexed by layerIndex
	trusts [2][]TrustEntry
}

var layerOrder = [3]Layer{LayerBundle, LayerPack, LayerBuiltin}

// NotFoundError is a reference no layer could resolve.
type NotFoundError struct {
	Ref string
	// Kind is "node" or "trust provider".
	Kind string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s %q not found in the bundle, installed packs or built-ins (node_not_found)", e.Kind, e.Ref)
}

// CodeNodeNotFound is the code of NotFoundError.
const CodeNodeNotFound = "node_not_found"

// IsNotFound reports whether err wraps a NotFoundError.
func IsNotFound(err error) bool {
	var nf *NotFoundError
	return errors.As(err, &nf)
}

// NewLocator builds a locator. A type declared twice inside one layer is an
// error (node_conflict), whether in two bundle files, two built-ins or two
// packs; the same type in different layers is fine, the higher layer wins.
func NewLocator(l Layers) (*Locator, error) {
	loc := &Locator{}
	var errs Errors
	for i, entries := range [][]Entry{l.Bundle, l.Packs, l.Builtin} {
		seen := map[string]Source{}
		for _, e := range entries {
			if first, dup := seen[e.Spec.Node]; dup {
				errs = append(errs, &Error{Code: CodeNodeConflict, Field: e.Spec.Node,
					Msg: fmt.Sprintf("%s in the %s layer is declared by %s and by %s", e.Spec.Node, layerOrder[i], first, e.Source)})
				continue
			}
			seen[e.Spec.Node] = e.Source
			loc.nodes[i] = append(loc.nodes[i], e)
		}
	}
	for i, entries := range [][]TrustEntry{l.BundleTrusts, l.PackTrusts} {
		seen := map[string]Source{}
		for _, e := range entries {
			if first, dup := seen[e.Provider.Trust]; dup {
				errs = append(errs, &Error{Code: CodeNodeConflict, Field: e.Provider.Trust,
					Msg: fmt.Sprintf("trust provider %s in the %s layer is declared by %s and by %s", e.Provider.Trust, layerOrder[i], first, e.Source)})
				continue
			}
			seen[e.Provider.Trust] = e.Source
			loc.trusts[i] = append(loc.trusts[i], e)
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return loc, nil
}

// Locate resolves ns/name@N, or ns/name for the highest major.
func (l *Locator) Locate(ref string) (*Entry, error) {
	base, _, pinned := strings.Cut(ref, "@")
	want := 0
	if pinned {
		t, err := ParseType(ref)
		if err != nil {
			return nil, &Error{Code: CodeInvalidNode, Field: "uses", Msg: err.Error()}
		}
		base, want = t.Ref(), t.Major
	} else if _, _, ok := strings.Cut(base, "/"); !ok {
		return nil, &Error{Code: CodeInvalidNode, Field: "uses", Msg: fmt.Sprintf("node reference %q: want ns/name[@N]", ref)}
	}
	for _, entries := range l.nodes {
		if e, ok := pick(entries, base, want); ok {
			return &e, nil
		}
	}
	return nil, &NotFoundError{Ref: ref, Kind: "node"}
}

func pick(entries []Entry, base string, major int) (Entry, bool) {
	var best Entry
	found := false
	for _, e := range entries {
		t, err := e.Spec.Type()
		if err != nil || t.Ref() != base {
			continue
		}
		if major != 0 {
			if t.Major == major {
				return e, true
			}
			continue
		}
		if !found || t.Major > bestMajor(best) {
			best, found = e, true
		}
	}
	return best, found
}

func bestMajor(e Entry) int {
	t, _ := e.Spec.Type()
	return t.Major
}

// Trust resolves a trust provider by name: bundle first, then packs.
func (l *Locator) Trust(name string) (*TrustEntry, error) {
	for _, entries := range l.trusts {
		for i := range entries {
			if entries[i].Provider.Trust == name {
				return &entries[i], nil
			}
		}
	}
	return nil, &NotFoundError{Ref: name, Kind: "trust provider"}
}

// List returns the effective node types: each ns/name@N once, from the
// highest layer that declares it, sorted by type.
func (l *Locator) List() []Entry {
	seen := map[string]bool{}
	var out []Entry
	for _, entries := range l.nodes {
		for _, e := range entries {
			if !seen[e.Spec.Node] {
				seen[e.Spec.Node] = true
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := out[i].Spec.Type()
		b, _ := out[j].Spec.Type()
		if a.Ref() != b.Ref() {
			return a.Ref() < b.Ref()
		}
		return a.Major < b.Major
	})
	return out
}

// LoadDir reads every node spec directly inside dir of fsys (YAML or JSON,
// not recursive), validating each, and attributes them to src with the
// file path filled in. A missing directory is not an error: an empty
// bundle has no nodes/.
func LoadDir(fsys fs.FS, dir string, src Source) ([]Entry, error) {
	files, err := docFiles(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []Entry
	var errs Errors
	for _, f := range files {
		data, err := fs.ReadFile(fsys, f)
		if err != nil {
			errs = append(errs, &Error{Code: CodeInvalidNode, Msg: fmt.Sprintf("read %s: %v", f, err)})
			continue
		}
		s, err := Load(strings.NewReader(string(data)), f)
		if err != nil {
			errs = append(errs, flatten(err)...)
			continue
		}
		e := src
		e.Path = f
		out = append(out, Entry{Spec: s, Source: e})
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}

// LoadTrustDir is LoadDir for trust providers.
func LoadTrustDir(fsys fs.FS, dir string, src Source) ([]TrustEntry, error) {
	files, err := docFiles(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []TrustEntry
	var errs Errors
	for _, f := range files {
		data, err := fs.ReadFile(fsys, f)
		if err != nil {
			errs = append(errs, &Error{Code: CodeInvalidNode, Msg: fmt.Sprintf("read %s: %v", f, err)})
			continue
		}
		t, err := LoadTrust(strings.NewReader(string(data)), f)
		if err != nil {
			errs = append(errs, flatten(err)...)
			continue
		}
		e := src
		e.Path = f
		out = append(out, TrustEntry{Provider: t, Source: e})
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}

// LoadBundle reads the nodes/ and trusts/ of a flow bundle rooted at the
// root of fsys (flow.md §2.4) into the bundle layer of Layers.
func LoadBundle(fsys fs.FS) (nodes []Entry, trusts []TrustEntry, err error) {
	src := Source{Layer: LayerBundle}
	if nodes, err = LoadDir(fsys, "nodes", src); err != nil {
		return nil, nil, err
	}
	if trusts, err = LoadTrustDir(fsys, "trusts", src); err != nil {
		return nil, nil, err
	}
	return nodes, trusts, nil
}

func docFiles(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && hasExt(e.Name(), docExts) {
			out = append(out, path.Join(dir, e.Name()))
		}
	}
	return out, nil
}
