package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Error codes of node-spec parsing. They are stable; messages are not.
const (
	// CodeInvalidNode is any structural problem with a node document.
	CodeInvalidNode = "invalid_node"
	// CodeUnknownKey is a key the node grammar does not define.
	CodeUnknownKey = "unknown_key"
)

// Rule ids of node.md §18 that Parse enforces.
const (
	RuleN01 = "N01" // required keys present
	RuleN02 = "N02" // type grammar
)

// Error is one problem found while decoding a node document.
type Error struct {
	// Code is CodeInvalidNode or CodeUnknownKey.
	Code string
	// Rule is the node.md §18 rule id (N01, N02), empty for a decode error.
	Rule string
	// Field is the key the problem is about, when there is one.
	Field string
	// Line is the 1-based line in the document, 0 when unknown.
	Line int
	Msg  string
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Line > 0 {
		fmt.Fprintf(&b, "line %d: ", e.Line)
	}
	if e.Rule != "" {
		fmt.Fprintf(&b, "%s: ", e.Rule)
	}
	b.WriteString(e.Msg)
	fmt.Fprintf(&b, " (%s)", e.Code)
	return b.String()
}

// Errors is every problem found in one document. It unwraps to its members.
type Errors []*Error

func (es Errors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "\n")
}

// Unwrap exposes the members to errors.Is and errors.As.
func (es Errors) Unwrap() []error {
	out := make([]error, len(es))
	for i, e := range es {
		out[i] = e
	}
	return out
}

// requiredKeys are the keys N01 demands. Presence is what counts: `inputs`
// and `capabilities` may be empty, which a decoded zero value could not tell
// from absent.
var requiredKeys = []string{"node", "kind", "inputs", "outputs", "capabilities", "side_effect", "privacy"}

// Parse decodes a node document and checks the rules of the grammar: N01
// (required keys present), N02 (type grammar), unknown keys (except x-*),
// and that enums hold a defined value. JSON is selected by a .json
// filename; anything else is YAML. The remaining rules N03..N15 are checks
// across fields and live with validation.
func Parse(r io.Reader, filename string) (*Spec, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read node spec %s: %w", filename, err)
	}
	spec, err := Decode(data, filename)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}
	return spec, nil
}

// Decode is Parse over bytes already read.
func Decode(data []byte, filename string) (*Spec, error) {
	if strings.EqualFold(filepath.Ext(filename), ".json") {
		// JSON is YAML; the YAML path gives key positions and strictness.
		if !json.Valid(data) {
			return nil, &Error{Code: CodeInvalidNode, Msg: "invalid json"}
		}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, &Error{Code: CodeInvalidNode, Msg: "invalid yaml: " + err.Error()}
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, &Error{Code: CodeInvalidNode, Msg: "a node spec is a mapping"}
	}

	var errs Errors
	ext := map[string]any{}
	kept := &yaml.Node{Kind: yaml.MappingNode, Tag: root.Tag, Line: root.Line}
	seen := map[string]bool{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		seen[k.Value] = true
		if strings.HasPrefix(k.Value, "x-") {
			var val any
			if err := v.Decode(&val); err != nil {
				errs = append(errs, &Error{Code: CodeInvalidNode, Field: k.Value, Line: k.Line, Msg: err.Error()})
				continue
			}
			ext[k.Value] = val
			continue
		}
		kept.Content = append(kept.Content, k, v)
	}
	for _, key := range requiredKeys {
		if !seen[key] {
			errs = append(errs, &Error{Code: CodeInvalidNode, Rule: RuleN01, Field: key, Line: root.Line,
				Msg: fmt.Sprintf("required key %q is missing", key)})
		}
	}

	// x-* keys are not known fields; without them the original bytes decode
	// strictly with true line numbers. With them the document is rebuilt
	// first and decode-error lines are approximate.
	var spec Spec
	body := data
	if len(ext) > 0 {
		b, merr := yaml.Marshal(kept)
		if merr != nil {
			return nil, &Error{Code: CodeInvalidNode, Msg: merr.Error()}
		}
		body = b
	}
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil && !errors.Is(err, io.EOF) {
		errs = append(errs, decodeErrors(err)...)
	}
	if len(ext) > 0 {
		spec.Ext = ext
	}

	errs = append(errs, checkEnums(&spec, seen)...)
	if len(errs) > 0 {
		sort.SliceStable(errs, func(i, j int) bool { return errs[i].Line < errs[j].Line })
		return nil, errs
	}
	sum := sha256.Sum256(data)
	spec.Path = filename
	spec.Hash = hex.EncodeToString(sum[:])
	return &spec, nil
}

func decodeErrors(err error) Errors {
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return Errors{{Code: CodeInvalidNode, Msg: err.Error()}}
	}
	out := make(Errors, 0, len(te.Errors))
	for _, msg := range te.Errors {
		code := CodeInvalidNode
		if strings.Contains(msg, "not found in type") {
			code = CodeUnknownKey
		}
		out = append(out, &Error{Code: code, Line: lineOf(msg), Msg: strings.TrimPrefix(msg, "line "+fmt.Sprint(lineOf(msg))+": ")})
	}
	return out
}

// lineOf reads the "line N:" prefix yaml.v3 puts on decode errors.
func lineOf(msg string) int {
	var n int
	if _, err := fmt.Sscanf(msg, "line %d:", &n); err != nil {
		return 0
	}
	return n
}

func checkEnums(s *Spec, seen map[string]bool) Errors {
	var errs Errors
	add := func(rule, field, msg string) {
		errs = append(errs, &Error{Code: CodeInvalidNode, Rule: rule, Field: field, Msg: msg})
	}
	if seen["node"] {
		if _, err := ParseType(s.Node); err != nil {
			add(RuleN02, "node", err.Error())
		}
	}
	if seen["kind"] && !s.Kind.Valid() {
		add("", "kind", fmt.Sprintf("kind %q: want command, http, builtin or plugin", s.Kind))
	}
	if s.Role != "" && s.Role != RoleStep && s.Role != RoleTrigger {
		add("", "role", fmt.Sprintf("role %q: want step or trigger", s.Role))
	}
	if s.LowersTo != "" && s.LowersTo != LowersExec && s.LowersTo != LowersEngine {
		add("", "lowers_to", fmt.Sprintf("lowers_to %q: want exec or engine", s.LowersTo))
	}
	if seen["side_effect"] && !s.SideEffect.Valid() {
		add("", "side_effect", fmt.Sprintf("side_effect %q: want none, read, write-local, write-shared or irreversible", s.SideEffect))
	}
	if seen["privacy"] && !s.Privacy.Valid() {
		add("", "privacy", fmt.Sprintf("privacy %q: want local or cloud", s.Privacy))
	}
	if s.Approval != "" && !s.Approval.Valid() {
		add("", "approval", fmt.Sprintf("approval %q: want none, required or trust", s.Approval))
	}
	if s.Cost != nil && !s.Cost.Class.Valid() {
		add("", "cost.class", fmt.Sprintf("cost class %q: want free, low, metered or expensive", s.Cost.Class))
	}
	return errs
}
