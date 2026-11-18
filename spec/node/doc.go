package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// decodeDoc strictly decodes one YAML or JSON document (JSON by .json file
// name) into out: root a mapping, unknown keys are errors, x-* keys of the
// root are returned in ext and kept out of the decode. required names keys
// whose presence is demanded. It is what Decode does for node specs, for
// the other documents of the package (packs, trust providers).
func decodeDoc(data []byte, filename, what string, out any, required []string) (ext map[string]any, errs Errors) {
	fail := func(msg string) Errors { return Errors{{Code: CodeInvalidNode, Msg: msg}} }
	if strings.EqualFold(filepath.Ext(filename), ".json") && !json.Valid(data) {
		return nil, fail("invalid json")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fail("invalid yaml: " + err.Error())
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, fail("a " + what + " is a mapping")
	}
	ext = map[string]any{}
	kept := &yaml.Node{Kind: yaml.MappingNode, Tag: root.Tag, Line: root.Line}
	seen := map[string]bool{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		seen[k.Value] = true
		if !strings.HasPrefix(k.Value, "x-") {
			kept.Content = append(kept.Content, k, v)
			continue
		}
		var val any
		if err := v.Decode(&val); err != nil {
			errs = append(errs, &Error{Code: CodeInvalidNode, Field: k.Value, Line: k.Line, Msg: err.Error()})
			continue
		}
		ext[k.Value] = val
	}
	for _, key := range required {
		if !seen[key] {
			errs = append(errs, &Error{Code: CodeInvalidNode, Field: key, Line: root.Line,
				Msg: fmt.Sprintf("required key %q is missing", key)})
		}
	}
	body := data
	if len(ext) > 0 {
		b, err := yaml.Marshal(kept)
		if err != nil {
			return nil, fail(err.Error())
		}
		body = b
	}
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		errs = append(errs, decodeErrors(err)...)
	}
	if len(ext) == 0 {
		ext = nil
	}
	return ext, errs
}
