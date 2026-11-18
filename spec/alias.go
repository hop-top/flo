package spec

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Aliases maps another vocabulary's document keys onto the flow spec's,
// so a host can accept its own wording (a host's "recipe:" for "flow:",
// "gate:" for "check:") without forking the parser. Spec owns the
// mechanism only; the wording belongs to the host.
//
// Documents are rewritten in place before parsing: a key is renamed on
// its own line, so line numbers in later errors stay true to the file.
// Anything the rewrite cannot decode is left untouched for the parser to
// report.
type Aliases struct {
	// Top renames keys of the document mapping, alias -> canonical.
	Top map[string]string
	// Step renames keys of every step mapping, including steps nested
	// under a step's "steps", alias -> canonical.
	Step map[string]string
	// Run renames the field of run.<field> inside {{...}} placeholders,
	// alias -> canonical.
	Run map[string]string
}

// IsZero reports an Aliases that renames nothing.
func (a Aliases) IsZero() bool { return len(a.Top) == 0 && len(a.Step) == 0 && len(a.Run) == 0 }

// Invert returns the reverse mapping, canonical -> alias: the one to
// apply when encoding a flow back into the host's vocabulary.
func (a Aliases) Invert() Aliases {
	return Aliases{Top: invertMap(a.Top), Step: invertMap(a.Step), Run: invertMap(a.Run)}
}

func invertMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[v] = k
	}
	return out
}

// ParseOptions tunes ParseFlowWith.
type ParseOptions struct {
	// Aliases, when non-nil, rewrites the document before it is parsed.
	// Flow.Hash is still the hash of the bytes as authored.
	Aliases *Aliases
}

// RawHash fingerprints raw document bytes the way Flow.Hash does:
// lowercase hex SHA-256.
func RawHash(data []byte) string {
	return flowHash(data)
}

const keySteps = "steps"

// Rewrite returns data with the aliases renamed to their canonical
// forms. filename selects JSON (a .json extension) or YAML. The input is
// not modified.
func (a Aliases) Rewrite(data []byte, filename string) []byte {
	if a.IsZero() {
		return data
	}
	data = a.rewriteRunRefs(data)
	if strings.EqualFold(filepath.Ext(filename), extJSON) {
		return a.rewriteJSON(data)
	}
	return a.rewriteYAML(data)
}

func (a Aliases) rewriteRunRefs(data []byte) []byte {
	if len(a.Run) == 0 {
		return data
	}
	names := sortedAliasKeys(a.Run)
	for i, n := range names {
		names[i] = regexp.QuoteMeta(n)
	}
	re := regexp.MustCompile(`(\{\{[^{}]*?\brun\.)(` + strings.Join(names, "|") + `)\b`)
	return re.ReplaceAllFunc(data, func(m []byte) []byte {
		sub := re.FindSubmatch(m)
		return append(append([]byte(nil), sub[1]...), a.Run[string(sub[2])]...)
	})
}

func sortedAliasKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RewriteNode renames aliased keys in a decoded YAML document in place.
// Use it with Invert to encode a flow in the host's vocabulary.
func (a Aliases) RewriteNode(root *yaml.Node) {
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return
	}
	for _, e := range a.collect(root) {
		e.node.Value = e.to
	}
}

type keyEdit struct {
	node      *yaml.Node
	line, col int
	from, to  string
}

func (a Aliases) rewriteYAML(data []byte) []byte {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return data
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return data
	}
	return applyKeyEdits(data, a.collect(root))
}

// collect queues the renames for the document mapping and every step
// mapping beneath it.
func (a Aliases) collect(root *yaml.Node) []keyEdit {
	var edits []keyEdit
	collectRenames(root, a.Top, &edits)
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == keySteps && root.Content[i+1].Kind == yaml.SequenceNode {
			a.collectSteps(root.Content[i+1], &edits)
		}
	}
	return edits
}

func (a Aliases) collectSteps(seq *yaml.Node, edits *[]keyEdit) {
	for _, step := range seq.Content {
		if step.Kind != yaml.MappingNode {
			continue
		}
		collectRenames(step, a.Step, edits)
		for i := 0; i+1 < len(step.Content); i += 2 {
			if step.Content[i].Value == keySteps && step.Content[i+1].Kind == yaml.SequenceNode {
				a.collectSteps(step.Content[i+1], edits)
			}
		}
	}
}

// collectRenames queues each rename of mapping m unless the canonical
// key is already present there.
func collectRenames(m *yaml.Node, renames map[string]string, edits *[]keyEdit) {
	for _, from := range sortedAliasKeys(renames) {
		to := renames[from]
		present := false
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == to {
				present = true
			}
		}
		for i := 0; i+1 < len(m.Content) && !present; i += 2 {
			if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Value == from {
				*edits = append(*edits, keyEdit{node: k, line: k.Line, col: k.Column, from: from, to: to})
			}
		}
	}
}

// applyKeyEdits renames keys in place on their own line. An edit whose
// key text is not at its position (flow-style oddities) is skipped.
func applyKeyEdits(data []byte, edits []keyEdit) []byte {
	if len(edits) == 0 {
		return data
	}
	lines := strings.Split(string(data), "\n")
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].line != edits[j].line {
			return edits[i].line < edits[j].line
		}
		return edits[i].col > edits[j].col
	})
	for _, e := range edits {
		if e.line < 1 || e.line > len(lines) {
			continue
		}
		line := lines[e.line-1]
		off := runeOffset(line, e.col-1)
		if off < 0 {
			continue
		}
		if off < len(line) && (line[off] == '"' || line[off] == '\'') {
			off++
		}
		if !strings.HasPrefix(line[off:], e.from) {
			continue
		}
		lines[e.line-1] = line[:off] + e.to + line[off+len(e.from):]
	}
	return []byte(strings.Join(lines, "\n"))
}

// runeOffset converts a 0-based rune column into a byte offset, or -1.
func runeOffset(s string, col int) int {
	n := 0
	for i := range s {
		if n == col {
			return i
		}
		n++
	}
	if n == col {
		return len(s)
	}
	return -1
}

func (a Aliases) rewriteJSON(data []byte) []byte {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return data
	}
	renameJSONKeys(doc, a.Top)
	if raw, ok := doc[keySteps]; ok {
		if steps, ok := a.rewriteJSONSteps(raw); ok {
			doc[keySteps] = steps
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return data
	}
	return out
}

func (a Aliases) rewriteJSONSteps(raw json.RawMessage) (json.RawMessage, bool) {
	var steps []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &steps); err != nil {
		return nil, false
	}
	for _, s := range steps {
		renameJSONKeys(s, a.Step)
		if nested, ok := s[keySteps]; ok {
			if out, ok := a.rewriteJSONSteps(nested); ok {
				s[keySteps] = out
			}
		}
	}
	out, err := json.Marshal(steps)
	if err != nil {
		return nil, false
	}
	return out, true
}

func renameJSONKeys(m map[string]json.RawMessage, renames map[string]string) {
	for _, from := range sortedAliasKeys(renames) {
		to := renames[from]
		v, ok := m[from]
		if !ok {
			continue
		}
		if _, clash := m[to]; clash {
			continue
		}
		delete(m, from)
		m[to] = v
	}
}
