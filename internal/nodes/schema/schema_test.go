package schema_test

import (
	"os"
	"path/filepath"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/schema"
	"hop.top/flo/spec/node"
)

func mustSpec(t *testing.T, y string) *node.Spec {
	t.Helper()
	s, err := node.Decode([]byte(y), "n.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const spec = `
node: x/y@1
kind: command
argv: [x]
inputs:
  repo: { type: string, required: true }
  limit: { type: integer, default: 10, minimum: 1, maximum: 100 }
  since: { type: [string, 'null'], format: date-time }
  lookback: { type: string, format: duration, default: 14d }
outputs:
  schema:
    type: object
    required: [n]
    properties: { n: { type: integer } }
capabilities: []
side_effect: read
privacy: local
`

func TestApplyInputs(t *testing.T) {
	s := mustSpec(t, spec)
	got, err := schema.ApplyInputs(s.Inputs, map[string]any{"repo": "a/b"})
	if err != nil {
		t.Fatal(err)
	}
	if got["limit"] != any(10) && got["limit"] != int(10) {
		t.Fatalf("limit = %#v", got["limit"])
	}
	if v, ok := got["since"]; !ok || v != nil {
		t.Fatalf("since = %#v ok=%v (nullable optional input resolves to null)", v, ok)
	}
	if got["lookback"] != "14d" {
		t.Fatalf("lookback = %v", got["lookback"])
	}
}

func TestApplyInputsInvalid(t *testing.T) {
	s := mustSpec(t, spec)
	for name, in := range map[string]map[string]any{
		"missing required": {},
		"wrong type":       {"repo": 5},
		"over maximum":     {"repo": "a", "limit": 101},
		"unknown input":    {"repo": "a", "extra": 1},
		"bad duration":     {"repo": "a", "lookback": "soon"},
		"bad date":         {"repo": "a", "since": "yesterday"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := schema.ApplyInputs(s.Inputs, in)
			if err == nil || err.Code != core.CodeInputInvalid {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestCheckOutput(t *testing.T) {
	s := mustSpec(t, spec)
	if err := schema.CheckOutput(s, map[string]any{"n": int64(3)}); err != nil {
		t.Fatal(err)
	}
	if err := schema.CheckOutput(s, map[string]any{"n": 3.0}); err != nil {
		t.Fatalf("integral float is an integer: %v", err)
	}
	err := schema.CheckOutput(s, map[string]any{"n": "x"})
	if err == nil || err.Code != core.CodeOutputInvalid {
		t.Fatalf("err = %v", err)
	}
	if err := schema.CheckOutput(s, []any{}); err == nil {
		t.Fatal("array is not an object")
	}
}

func TestOutputSchemaFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "out.json"), []byte(`{"type":"array"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := mustSpec(t, "node: x/z@1\nkind: command\nargv: [x]\ninputs: {}\noutputs: { schema: out.json }\ncapabilities: []\nside_effect: read\nprivacy: local\n")
	s.Path = filepath.Join(dir, "z.yaml")
	if err := schema.CheckOutput(s, []any{}); err != nil {
		t.Fatal(err)
	}
	if err := schema.CheckOutput(s, map[string]any{}); err == nil {
		t.Fatal("want output_invalid")
	}
	s.Outputs.Schema = "../escape.json"
	if err := schema.CheckOutput(s, []any{}); err == nil || err.Code != core.CodeInvalidNode {
		t.Fatalf("escape: %v", err)
	}
}
