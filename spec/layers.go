package spec

import (
	"bytes"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// Layer is one place flows live, read through an fs.FS (its root holds
// the flow files). Layers keep the host's own directories and embedded
// sets apart while sharing one lookup.
type Layer struct {
	// Source labels the layer in Flow.Source, FlowInfo.Source and
	// not-found errors: a directory, or BuiltinSource.
	Source string
	// FS serves the flow files at its root.
	FS fs.FS
	// Dir is the real directory behind FS, if any. When set, Path is
	// Dir joined with the file name, so a result points at the real
	// file; when empty, Locate reports "<Source>:<name>" and List the
	// bare name.
	Dir string
}

// LayerLocator resolves flows from ordered fs layers. An unpinned name
// takes the first layer that has it (highest version there); a pinned
// name@version is searched across every layer in order. A path is opened
// directly from the OS file system.
//
// Aliases, when set, apply to every document read, headers included, and
// Flow.Hash is always the hash of the bytes as stored. It implements
// Locator.
type LayerLocator struct {
	Layers  []Layer
	Aliases *Aliases
}

func (l *LayerLocator) opts() ParseOptions { return ParseOptions{Aliases: l.Aliases} }

// Locate resolves ref: a file path, a name, or name@version.
func (l *LayerLocator) Locate(ref string) (*Flow, error) {
	if isFlowPath(ref) {
		return loadFlowFileWith(ref, filepath.Dir(ref), l.opts())
	}
	name, version, _ := strings.Cut(ref, "@")
	if !flowNameRe.MatchString(name) {
		return nil, fmt.Errorf("invalid flow reference %q: want a name, name@version or a file path", ref)
	}
	for _, layer := range l.Layers {
		info, ok := pickFlow(l.scan(layer), name, version)
		if !ok {
			continue
		}
		return l.load(layer, info)
	}
	sources := make([]string, len(l.Layers))
	for i, layer := range l.Layers {
		sources[i] = layer.Source
	}
	return nil, &FlowNotFoundError{Ref: ref, Dirs: sources}
}

// List returns every flow header, in layer order, then by name and
// descending version within a layer. Unreadable layers are skipped.
func (l *LayerLocator) List() ([]FlowInfo, error) {
	var out []FlowInfo
	for _, layer := range l.Layers {
		out = append(out, l.scan(layer)...)
	}
	return out, nil
}

// scan lists one layer's headers; Path is the layer-local name until
// load or List-time joining.
func (l *LayerLocator) scan(layer Layer) []FlowInfo {
	entries, err := fs.ReadDir(layer.FS, ".")
	if err != nil {
		return nil
	}
	var infos []FlowInfo
	for _, e := range entries {
		if e.IsDir() || !isFlowExt(e.Name()) {
			continue
		}
		data, err := fs.ReadFile(layer.FS, e.Name())
		if err != nil {
			continue
		}
		if l.Aliases != nil {
			data = l.Aliases.Rewrite(data, e.Name())
		}
		info, ok := headerInfo(data, layer.Source, e.Name())
		if !ok {
			continue
		}
		if layer.Dir != "" {
			info.Path = filepath.Join(layer.Dir, e.Name())
		}
		infos = append(infos, info)
	}
	sortFlowInfos(infos)
	return infos
}

func (l *LayerLocator) load(layer Layer, info FlowInfo) (*Flow, error) {
	name := filepath.Base(info.Path)
	data, err := fs.ReadFile(layer.FS, name)
	if err != nil {
		return nil, fmt.Errorf("read flow %s: %w", name, err)
	}
	path := layer.Source + ":" + name
	if layer.Dir != "" {
		path = filepath.Join(layer.Dir, name)
	}
	f, err := ParseFlowWith(bytes.NewReader(data), path, l.opts())
	if err != nil {
		return nil, err
	}
	f.Source = layer.Source
	return f, nil
}
