package node

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parseString(t *testing.T, doc, name string) (*Spec, error) {
	t.Helper()
	return Parse(strings.NewReader(doc), name)
}

// Every node spec of docs/spec/examples.md, extracted verbatim, parses.
func TestParseExamples(t *testing.T) {
	files, err := filepath.Glob("testdata/examples/*.yaml")
	if err != nil || len(files) != 12 {
		t.Fatalf("glob = %v, %v; want the 12 node specs of examples.md", files, err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f) //nolint:gosec // fixture path from a fixed glob
			if err != nil {
				t.Fatal(err)
			}
			s, err := Parse(bytes.NewReader(data), f)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if _, err := s.Type(); err != nil {
				t.Errorf("Type: %v", err)
			}
			if s.Hash == "" || s.Path != f {
				t.Errorf("Path/Hash not set: %q %q", s.Path, s.Hash)
			}
		})
	}
}

func TestParseExampleFields(t *testing.T) {
	data, err := os.ReadFile("testdata/examples/gh_dependency-list_1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(bytes.NewReader(data), "dep.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ty, _ := s.Type()
	if ty != (Type{NS: "gh", Name: "dependency-list", Major: 1}) || ty.String() != "gh/dependency-list@1" {
		t.Errorf("type = %+v", ty)
	}
	if s.Kind != KindCommand || s.SideEffect != SideEffectRead || s.Privacy != PrivacyCloud {
		t.Errorf("header = %s %s %s", s.Kind, s.SideEffect, s.Privacy)
	}
	if s.ExitCodes == nil || len(s.ExitCodes.Retry) != 1 || s.ExitCodes.Retry[0] != 75 {
		t.Errorf("exit_codes = %+v", s.ExitCodes)
	}
	if s.RateLimit == nil || s.RateLimit.Bucket != "vcs-dependency-export" || s.RateLimit.Max != 100 || s.RateLimit.Per != "1h" {
		t.Errorf("rate_limit = %+v", s.RateLimit)
	}
	if s.Outputs.From != "file:out.json" || s.Outputs.Format != "json" {
		t.Errorf("outputs = %+v", s.Outputs)
	}
	repos := s.Inputs["repos"]
	if !repos.Required.Flag || len(repos.Type) != 1 || repos.Type[0] != "array" || repos.Items == nil {
		t.Errorf("inputs.repos = %+v", repos)
	}
	if got := s.Inputs["mode"]; got.Default != "sbom" || len(got.Enum) != 2 {
		t.Errorf("inputs.mode = %+v", got)
	}
}

func TestTypeListAcceptsScalarAndList(t *testing.T) {
	doc := minimalDoc + "\ninputs:\n  q: { type: [string, array], required: true }\n  n: { type: integer }\n"
	doc = strings.Replace(doc, "inputs: {}\n", "", 1)
	s, err := parseString(t, doc, "n.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Inputs["q"].Type; len(got) != 2 || got[1] != "array" {
		t.Errorf("q type = %v", got)
	}
	if got := s.Inputs["n"].Type; len(got) != 1 || got[0] != "integer" {
		t.Errorf("n type = %v", got)
	}
}

const minimalDoc = `node: ns/name@1
kind: command
argv: [true]
inputs: {}
outputs: { schema: { type: object } }
capabilities: []
side_effect: none
privacy: local
`

func TestParseMinimalAndDefaults(t *testing.T) {
	s, err := parseString(t, minimalDoc, "m.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if s.Inputs == nil && len(s.Inputs) != 0 {
		t.Error("empty inputs must decode")
	}
	if !s.EffectiveIdempotent() || s.EffectiveApproval() != ApprovalNone || s.EffectiveLowersTo() != LowersExec ||
		s.EffectiveRole() != RoleStep || !s.EffectiveRecord() {
		t.Errorf("defaults wrong: %v %v %v %v", s.EffectiveIdempotent(), s.EffectiveApproval(), s.EffectiveLowersTo(), s.EffectiveRole())
	}
	if p := s.EffectivePlacement(); p.Runner != RunnerLocal || p.Isolation != IsolationNone {
		t.Errorf("placement = %+v", p)
	}
	irr := *s
	irr.SideEffect = SideEffectIrreversible
	if irr.EffectiveIdempotent() || irr.EffectiveApproval() != ApprovalRequired {
		t.Error("irreversible defaults: not idempotent, approval required")
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name     string
		doc      string
		wantCode string
		wantRule string
		wantText string
	}{
		{"unknown key", minimalDoc + "bogus: 1\n", CodeUnknownKey, "", "bogus"},
		{"unknown nested key", strings.Replace(minimalDoc, "outputs: { schema: { type: object } }", "outputs: { schema: { type: object }, frm: stdout }", 1), CodeUnknownKey, "", "frm"},
		{"N01 missing node", strings.Replace(minimalDoc, "node: ns/name@1\n", "", 1), CodeInvalidNode, RuleN01, `"node"`},
		{"N01 missing inputs", strings.Replace(minimalDoc, "inputs: {}\n", "", 1), CodeInvalidNode, RuleN01, `"inputs"`},
		{"N01 missing capabilities", strings.Replace(minimalDoc, "capabilities: []\n", "", 1), CodeInvalidNode, RuleN01, `"capabilities"`},
		{"N01 missing side_effect", strings.Replace(minimalDoc, "side_effect: none\n", "", 1), CodeInvalidNode, RuleN01, `"side_effect"`},
		{"N01 missing privacy", strings.Replace(minimalDoc, "privacy: local\n", "", 1), CodeInvalidNode, RuleN01, `"privacy"`},
		{"N02 no major", strings.Replace(minimalDoc, "ns/name@1", "ns/name", 1), CodeInvalidNode, RuleN02, "ns/name@N"},
		{"N02 bad major", strings.Replace(minimalDoc, "ns/name@1", "ns/name@0", 1), CodeInvalidNode, RuleN02, "positive integer"},
		{"N02 bad ns", strings.Replace(minimalDoc, "ns/name@1", "Ns/name@1", 1), CodeInvalidNode, RuleN02, "namespace"},
		{"N02 bad name", strings.Replace(minimalDoc, "ns/name@1", "ns/na_me@1", 1), CodeInvalidNode, RuleN02, "name"},
		{"bad kind", strings.Replace(minimalDoc, "kind: command", "kind: shell", 1), CodeInvalidNode, "", "kind"},
		{"bad side effect", strings.Replace(minimalDoc, "side_effect: none", "side_effect: read-write", 1), CodeInvalidNode, "", "side_effect"},
		{"bad privacy", strings.Replace(minimalDoc, "privacy: local", "privacy: public", 1), CodeInvalidNode, "", "privacy"},
		{"not a mapping", "- a\n- b\n", CodeInvalidNode, "", "mapping"},
		{"bad yaml", "node: [\n", CodeInvalidNode, "", "yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseString(t, tt.doc, "bad.yaml")
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q lacks %q", err, tt.wantText)
			}
			var ne *Error
			if !errors.As(err, &ne) {
				t.Fatalf("error %T is not a *node.Error", err)
			}
			found := false
			var es Errors
			if errors.As(err, &es) {
				for _, e := range es {
					if e.Code == tt.wantCode && e.Rule == tt.wantRule {
						found = true
					}
				}
			} else {
				found = ne.Code == tt.wantCode && ne.Rule == tt.wantRule
			}
			if !found {
				t.Errorf("no error with code %q rule %q in %v", tt.wantCode, tt.wantRule, err)
			}
		})
	}
}

func TestParseKeepsExtensionKeys(t *testing.T) {
	s, err := parseString(t, minimalDoc+"x-owner: inbox\nx-notes: { a: 1 }\n", "x.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if s.Ext["x-owner"] != "inbox" || s.Ext["x-notes"] == nil {
		t.Errorf("ext = %v", s.Ext)
	}
}

func TestParseJSON(t *testing.T) {
	doc := `{"node":"ns/name@2","kind":"builtin","inputs":{},"outputs":{"schema":{"type":"object"}},"capabilities":[],"side_effect":"none","privacy":"local"}`
	s, err := parseString(t, doc, "n.json")
	if err != nil {
		t.Fatal(err)
	}
	if s.Node != "ns/name@2" || s.Kind != KindBuiltin {
		t.Errorf("spec = %+v", s)
	}
	if _, err := parseString(t, "{not json", "n.json"); err == nil {
		t.Error("invalid json must fail")
	}
}

func TestSideEffectLadder(t *testing.T) {
	order := []SideEffect{SideEffectNone, SideEffectRead, SideEffectWriteLocal, SideEffectWriteShared, SideEffectIrreversible}
	for i, a := range order {
		for j, b := range order {
			if got := a.AtLeast(b); got != (i >= j) {
				t.Errorf("%s.AtLeast(%s) = %v", a, b, got)
			}
		}
	}
	if SideEffect("bogus").Rank() != SideEffectIrreversible.Rank() {
		t.Error("an unknown class ranks as irreversible")
	}
}

func TestParseTypeTable(t *testing.T) {
	ok := map[string]Type{
		"flo/dedupe@1":             {"flo", "dedupe", 1},
		"heo/conversation.list@12": {"heo", "conversation.list", 12},
		"a-b/c-d.e-f@3":            {"a-b", "c-d.e-f", 3},
	}
	for in, want := range ok {
		got, err := ParseType(in)
		if err != nil || got != want {
			t.Errorf("ParseType(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "flo/dedupe", "dedupe@1", "flo/@1", "/x@1", "flo/x@01", "flo/x@-1", "flo/x@a", "1a/x@1", "flo/x.@1", "flo/.x@1", "meeting-brief@0.1.0"} {
		if _, err := ParseType(in); err == nil {
			t.Errorf("ParseType(%q) accepted", in)
		}
	}
}
