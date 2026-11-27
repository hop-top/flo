package spec

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ExtPrefix starts the keys of implementer-owned extension data. A flow
// document and each of its steps may carry any number of keys with this
// prefix (flow spec §2.2). The spec parses and preserves them and gives
// them no meaning; the node spec reserves the same prefix at its root.
const ExtPrefix = "x-"

func isExtKey(k string) bool { return strings.HasPrefix(k, ExtPrefix) }

// extTree is the extension data pulled out of a document, shaped like the
// document: this level's keys and one entry per step of its steps list.
type extTree struct {
	ext   map[string]any
	steps []extTree
}

func (t extTree) any() bool {
	if len(t.ext) > 0 {
		return true
	}
	return slices.ContainsFunc(t.steps, extTree.any)
}

// apply stores the extensions on the flow and its steps. Steps are matched
// by position: the kept document has the same lists as the original.
func (t extTree) apply(r *Flow) {
	r.Ext = t.ext
	applyStepExt(t.steps, r.Steps)
}

func applyStepExt(trees []extTree, steps []Step) {
	for i := range steps {
		if i >= len(trees) {
			return
		}
		steps[i].Ext = trees[i].ext
		applyStepExt(trees[i].steps, steps[i].Steps)
	}
}

// splitExtYAML removes the x-* entries of the flow mapping and of every
// step mapping beneath it from the node tree and returns them decoded.
func splitExtYAML(m *yaml.Node) (extTree, error) {
	var t extTree
	kept := m.Content[:0:0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		switch {
		case isExtKey(k.Value):
			var val any
			if err := v.Decode(&val); err != nil {
				return t, fmt.Errorf("line %d: %s: %w", k.Line, k.Value, err)
			}
			if t.ext == nil {
				t.ext = map[string]any{}
			}
			t.ext[k.Value] = val
			continue
		case k.Value == keySteps && v.Kind == yaml.SequenceNode:
			for _, item := range v.Content {
				child := extTree{}
				if item.Kind == yaml.MappingNode {
					var err error
					if child, err = splitExtYAML(item); err != nil {
						return t, err
					}
				}
				t.steps = append(t.steps, child)
			}
		}
		kept = append(kept, k, v)
	}
	m.Content = kept
	return t, nil
}

// splitExtJSON is splitExtYAML over a decoded JSON object. Values are
// decoded as YAML, which JSON is, so numbers come out as they do for a
// YAML document.
func splitExtJSON(m map[string]json.RawMessage) (extTree, error) {
	var t extTree
	for k, raw := range m {
		if !isExtKey(k) {
			continue
		}
		var val any
		if err := yaml.Unmarshal(raw, &val); err != nil {
			return t, fmt.Errorf("%s: %w", k, err)
		}
		if t.ext == nil {
			t.ext = map[string]any{}
		}
		t.ext[k] = val
		delete(m, k)
	}
	raw, ok := m[keySteps]
	if !ok {
		return t, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return t, nil // not a list: the strict decode reports it
	}
	for i, item := range items {
		var obj map[string]json.RawMessage
		if json.Unmarshal(item, &obj) != nil {
			t.steps = append(t.steps, extTree{})
			continue
		}
		child, err := splitExtJSON(obj)
		if err != nil {
			return t, err
		}
		t.steps = append(t.steps, child)
		b, err := json.Marshal(obj)
		if err != nil {
			return t, fmt.Errorf("steps[%d]: %w", i, err)
		}
		items[i] = b
	}
	b, err := json.Marshal(items)
	if err != nil {
		return t, fmt.Errorf("steps: %w", err)
	}
	m[keySteps] = b
	return t, nil
}

// validateExtKeys refuses extension keys without the prefix, so a flow
// built in code serializes to a document that parses again.
func validateExtKeys(where string, ext map[string]any) error {
	for k := range ext {
		if !isExtKey(k) {
			return fmt.Errorf("%s: extension key %q must start with %q", where, k, ExtPrefix)
		}
	}
	return nil
}

// MarshalYAML writes the flow with its extension keys after the document
// keys, in key order.
func (r Flow) MarshalYAML() (any, error) {
	type plain Flow
	return encodeExtYAML(plain(r), r.Ext)
}

// MarshalYAML writes the step with its extension keys after the step keys,
// in key order.
func (s Step) MarshalYAML() (any, error) {
	type plain Step
	return encodeExtYAML(plain(s), s.Ext)
}

func encodeExtYAML(v any, ext map[string]any) (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	keys := make([]string, 0, len(ext))
	for k := range ext {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var val yaml.Node
		if err := val.Encode(ext[k]); err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, &val)
	}
	return &n, nil
}

// MarshalJSON writes the flow with its extension keys merged into the
// object.
func (r Flow) MarshalJSON() ([]byte, error) {
	type plain Flow
	return encodeExtJSON(plain(r), r.Ext)
}

// MarshalJSON writes the step with its extension keys merged into the
// object.
func (s Step) MarshalJSON() ([]byte, error) {
	type plain Step
	return encodeExtJSON(plain(s), s.Ext)
}

func encodeExtJSON(v any, ext map[string]any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil || len(ext) == 0 {
		return b, err //nolint:wrapcheck // plain marshal of the document type
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	for k, e := range ext {
		if obj[k], err = json.Marshal(e); err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
	}
	return json.Marshal(obj) //nolint:wrapcheck // plain marshal of raw messages
}
