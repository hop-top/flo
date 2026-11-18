package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseFlow decodes a flow document and validates it. JSON is selected
// by a .json filename; anything else is YAML. Unknown keys are errors so a
// misspelled field never silently drops out of a task. Validation always
// runs — there is no extension that skips it.
func ParseFlow(r io.Reader, filename string) (*Flow, error) {
	return ParseFlowWith(r, filename, ParseOptions{})
}

// ParseFlowWith is ParseFlow with options. The zero options behave
// exactly as ParseFlow. Flow.Hash is always the hash of the bytes read,
// before any alias rewrite.
func ParseFlowWith(r io.Reader, filename string, opts ParseOptions) (*Flow, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read flow %s: %w", filename, err)
	}
	src := data
	if opts.Aliases != nil {
		src = opts.Aliases.Rewrite(data, filename)
	}
	var rec Flow
	if strings.EqualFold(filepath.Ext(filename), extJSON) {
		err = decodeFlowJSON(src, &rec)
	} else {
		err = decodeFlowYAML(src, &rec)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}
	rec.Path = filename
	rec.Hash = flowHash(data)
	for i := range rec.Steps {
		rec.Steps[i].Ordinal = i + 1
	}
	if err := ValidateFlow(&rec); err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}
	return &rec, nil
}

// flowHash fingerprints the raw document so drift between a flow and
// the tasks it produced is detectable even without a version bump.
func flowHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func decodeFlowYAML(data []byte, rec *Flow) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("invalid yaml: %w", err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != 0 && root.Kind != yaml.MappingNode {
		return fmt.Errorf("a flow is a mapping with flow, version and steps keys")
	}
	if err := rejectUnknownShapeYAML(root); err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(rec); err != nil && err != io.EOF { //nolint:errorlint // yaml returns the sentinel itself
		return fmt.Errorf("invalid flow: %w", err)
	}
	return nil
}

// rejectUnknownShapeYAML refuses documents shaped like something other
// than a flow, pointing at the keys a flow uses instead of decoding
// them into an empty one.
func rejectUnknownShapeYAML(root *yaml.Node) error {
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, val := root.Content[i], root.Content[i+1]
		if err := rejectUnknownKey(key.Value, val.Kind == yaml.MappingNode); err != nil {
			return fmt.Errorf("line %d: %w", key.Line, err)
		}
	}
	return nil
}

// Keys no flow has. They are refused by name rather than by the
// unknown-field error so the message can say what to write instead.
const (
	rejectedKeyID     = "flow_id"
	rejectedKeyEntry  = "entry_step"
	rejectedKeyRecipe = "recipe"
	keyStages         = "stages"
)

func rejectUnknownKey(key string, stepsIsMap bool) error {
	switch key {
	case rejectedKeyID, rejectedKeyEntry, rejectedKeyRecipe:
		return fmt.Errorf("unknown key %q: name the document with \"flow:\" and list its steps under \"steps:\"", key)
	case nsSubject, keyStages:
		return fmt.Errorf("reserved_key: %q is reserved for a later version of the flow spec", key)
	case nsSteps:
		if stepsIsMap {
			return fmt.Errorf("a flow's steps must be a list of steps with ids, not a map")
		}
	}
	return nil
}

func decodeFlowJSON(data []byte, rec *Flow) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("invalid json: %w", err)
	}
	for key, val := range raw {
		trimmed := bytes.TrimSpace(val)
		if err := rejectUnknownKey(key, len(trimmed) > 0 && trimmed[0] == '{'); err != nil {
			return err
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(rec); err != nil {
		return fmt.Errorf("invalid flow: %w", err)
	}
	return nil
}
