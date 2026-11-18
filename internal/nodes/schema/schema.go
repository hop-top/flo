// Package schema validates a node's inputs and outputs against its spec
// (node.md §3): the JSON-Schema subset of `inputs`, the full JSON Schema of
// `outputs.schema`. A mismatch on the way in is input_invalid, on the way
// out output_invalid.
package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec"
	"hop.top/flo/spec/node"
)

func init() {
	// node.md §3.1: `format: duration` is the flow duration (30s, 5d, 2w),
	// not ISO 8601.
	jsonschema.Formats["duration"] = func(v any) bool {
		s, ok := v.(string)
		if !ok {
			return true
		}
		_, err := spec.ParseDuration(s)
		return err == nil
	}
}

// maxViolations bounds the violations listed in an error.
const maxViolations = 10

var (
	cacheMu sync.Mutex
	cache   = map[[sha256.Size]byte]*jsonschema.Schema{}
)

func compile(doc any) (*jsonschema.Schema, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("schema is not JSON: %w", err)
	}
	key := sha256.Sum256(b)
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if s, ok := cache[key]; ok {
		return s, nil
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat = true
	const url = "mem://node/schema.json"
	if err := c.AddResource(url, bytes.NewReader(b)); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	s, err := c.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	cache[key] = s
	return s, nil
}

// normalize turns a Go value into the JSON model the validator reads
// (numbers as json.Number, maps and lists as plain Go), copying it.
func normalize(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	return out, nil
}

func violations(err error) []string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []string{err.Error()}
	}
	var out []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := e.InstanceLocation
			if loc == "" {
				loc = "/"
			}
			out = append(out, loc+": "+e.Message)
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	sort.Strings(out)
	if len(out) > maxViolations {
		out = append(out[:maxViolations], fmt.Sprintf("... %d more", len(out)-maxViolations))
	}
	return out
}

// InputSchema returns the JSON Schema of a node's inputs. Undeclared inputs
// are refused: a typo in `with` must not pass silently.
func InputSchema(inputs map[string]node.InputDef) map[string]any {
	s := objectSchema(inputs, nil)
	s["additionalProperties"] = false
	return s
}

func objectSchema(props map[string]node.InputDef, names []string) map[string]any {
	out := map[string]any{"type": "object"}
	p := make(map[string]any, len(props))
	req := append([]string(nil), names...)
	for n, d := range props {
		p[n] = defSchema(d)
		if d.Required.Flag {
			req = append(req, n)
		}
	}
	out["properties"] = p
	if len(req) > 0 {
		sort.Strings(req)
		out["required"] = req
	}
	return out
}

func defSchema(d node.InputDef) map[string]any {
	s := map[string]any{}
	switch len(d.Type) {
	case 0:
	case 1:
		s["type"] = d.Type[0]
	default:
		s["type"] = []string(d.Type)
	}
	if len(d.Enum) > 0 {
		s["enum"] = d.Enum
	}
	if d.Minimum != nil {
		s["minimum"] = *d.Minimum
	}
	if d.Maximum != nil {
		s["maximum"] = *d.Maximum
	}
	if d.Format != "" {
		s["format"] = d.Format
	}
	if d.Items != nil {
		s["items"] = defSchema(*d.Items)
	}
	if len(d.Properties) > 0 {
		for k, v := range objectSchema(d.Properties, d.Required.Names) {
			if k != "type" {
				s[k] = v
			}
		}
	}
	return s
}

// ApplyInputs returns in with the declared defaults applied, after checking
// it against the inputs of spec. An optional input that allows null and has
// no default is set to null, so a template reading it resolves. The error is
// input_invalid, its Detail listing the violations.
func ApplyInputs(inputs map[string]node.InputDef, in map[string]any) (map[string]any, *core.Error) {
	out := make(map[string]any, len(in)+len(inputs))
	for k, v := range in {
		out[k] = v
	}
	for name, d := range inputs {
		if _, ok := out[name]; ok {
			continue
		}
		switch {
		case d.HasDefault():
			out[name] = d.Default
		case !d.Required.Flag && allowsNull(d):
			out[name] = nil
		}
	}
	sc, err := compile(InputSchema(inputs))
	if err != nil {
		return nil, core.Errorf(core.CodeInvalidNode, "inputs schema: %v", err)
	}
	doc, err := normalize(out)
	if err != nil {
		return nil, core.Errorf(core.CodeInputInvalid, "inputs: %v", err)
	}
	if err := sc.Validate(doc); err != nil {
		v := violations(err)
		return nil, core.Errorf(core.CodeInputInvalid, "inputs do not match the node: %s", strings.Join(v, "; ")).
			WithDetail("violations", v)
	}
	return out, nil
}

func allowsNull(d node.InputDef) bool {
	for _, t := range d.Type {
		if t == "null" {
			return true
		}
	}
	return false
}

// OutputSchema returns the JSON Schema document of a node's output, loading
// a schema file named in the spec relative to the spec's own file. ok is
// false when the node declares none.
func OutputSchema(s *node.Spec) (doc any, ok bool, err error) {
	switch t := s.Outputs.Schema.(type) {
	case nil:
		return nil, false, nil
	case string:
		if s.Path == "" {
			return nil, false, fmt.Errorf("outputs.schema %q is a file but the spec has no path", t)
		}
		if !filepath.IsLocal(t) {
			return nil, false, fmt.Errorf("outputs.schema %q must stay inside the node's directory", t)
		}
		b, rerr := os.ReadFile(filepath.Join(filepath.Dir(s.Path), t)) //nolint:gosec // path of a node's own schema file
		if rerr != nil {
			return nil, false, fmt.Errorf("outputs.schema: %w", rerr)
		}
		var d any
		if jerr := json.Unmarshal(b, &d); jerr != nil {
			return nil, false, fmt.Errorf("outputs.schema %q: %w", t, jerr)
		}
		return d, true, nil
	default:
		return t, true, nil
	}
}

// CheckOutput validates the value a node produced against outputs.schema.
// A node with no schema accepts any value. The error is output_invalid.
func CheckOutput(s *node.Spec, value any) *core.Error {
	doc, ok, err := OutputSchema(s)
	if err != nil {
		return core.Errorf(core.CodeInvalidNode, "%s: %v", s.Node, err)
	}
	if !ok {
		return nil
	}
	sc, err := compile(doc)
	if err != nil {
		return core.Errorf(core.CodeInvalidNode, "%s: output %v", s.Node, err)
	}
	v, err := normalize(value)
	if err != nil {
		return core.Errorf(core.CodeOutputInvalid, "%s: output: %v", s.Node, err)
	}
	if err := sc.Validate(v); err != nil {
		vs := violations(err)
		return core.Errorf(core.CodeOutputInvalid, "%s: output does not match its schema: %s", s.Node, strings.Join(vs, "; ")).
			WithDetail("violations", vs)
	}
	return nil
}
