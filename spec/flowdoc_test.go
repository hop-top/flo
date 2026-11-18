package spec

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// examplesPath is the worked-examples document of the flow spec, a
// sibling of this module in the repository.
var examplesPath = filepath.Join("..", "docs", "spec", "examples.md")

var yamlFence = regexp.MustCompile("(?s)```yaml\n(.*?)```")

// exampleFlows returns the fenced YAML blocks of examples.md that are flow
// documents (they start with a top-level flow: key); node specs and trust
// providers in the same file are other documents.
func exampleFlows(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(examplesPath)
	if err != nil {
		t.Fatalf("read %s: %v", examplesPath, err)
	}
	out := map[string]string{}
	for _, m := range yamlFence.FindAllStringSubmatch(string(data), -1) {
		body := m[1]
		if !strings.HasPrefix(body, "flow: ") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(body, "flow: "), "\n")
		out[strings.TrimSpace(name)] = body
	}
	return out
}

func TestExamples_FlowsParse(t *testing.T) {
	flows := exampleFlows(t)
	for _, want := range []string{"dependency-watch", "meeting-brief", "inbox-triage"} {
		body, ok := flows[want]
		if !ok {
			t.Fatalf("examples.md has no flow %q (found %d flows)", want, len(flows))
		}
		t.Run(want, func(t *testing.T) {
			f, err := ParseFlow(strings.NewReader(body), want+".yaml")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if f.Name != want || f.Version == "" || len(f.Steps) == 0 {
				t.Errorf("flow = %q %q, %d steps", f.Name, f.Version, len(f.Steps))
			}
			if f.Hash == "" {
				t.Error("no content hash")
			}
		})
	}
	if len(flows) != 3 {
		t.Errorf("examples.md has %d flows; the test names 3", len(flows))
	}
}

func TestExamples_RunLevelKeysKept(t *testing.T) {
	body := exampleFlows(t)["dependency-watch"]
	f, err := ParseFlow(strings.NewReader(body), "dependency-watch.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.On) != 2 || f.RunAs != "automation" || f.Privacy != "cloud-allowed" || f.Tasks != "none" {
		t.Errorf("run-level keys lost: on=%v run_as=%q privacy=%q tasks=%q", f.On, f.RunAs, f.Privacy, f.Tasks)
	}
	if _, ok := f.State["seen"]; !ok {
		t.Errorf("state not kept: %v", f.State)
	}
	if f.Vars["repos"].Type != "list" {
		t.Errorf("var type not kept: %+v", f.Vars["repos"])
	}
	byID := map[string]Step{}
	for _, s := range f.Steps {
		byID[s.ID] = s
	}
	rel := byID["releases"]
	if !rel.IsNode() || rel.Concurrency != 4 || rel.OnItemError != "skip" || rel.ForEach == "" {
		t.Errorf("node step keys lost: %+v", rel)
	}
	if byID["judge"].Check == nil || byID["judge"].Check.Contract != "dep-judge-v1" {
		t.Errorf("check lost: %+v", byID["judge"].Check)
	}
	if byID["newadv"].Outputs["state"] == nil {
		t.Errorf("outputs lost: %+v", byID["newadv"].Outputs)
	}
}

func TestExamples_ResultRefsAreImplicitEdges(t *testing.T) {
	body := exampleFlows(t)["dependency-watch"]
	f, err := ParseFlow(strings.NewReader(body), "dependency-watch.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range f.Steps {
		if s.ID != "newadv" {
			continue
		}
		deps := ImplicitDeps(s)
		if len(deps) != 1 || deps[0] != "advisories" {
			t.Errorf("implicit deps of newadv = %v; want [advisories]", deps)
		}
		return
	}
	t.Fatal("no newadv step")
}

func TestParseFlow_RunLevelRules(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"recipe key refused", "recipe: x\nversion: 1\nsteps:\n  - id: a\n", `"flow:"`},
		{"gate key is unknown", "flow: x\nversion: 1\nsteps:\n  - id: a\n    gate: {contract: c}\n", "gate"},
		{"reserved subject", "flow: x\nversion: 1\nsubject: {kind: client}\nsteps:\n  - id: a\n", "reserved_key"},
		{"reserved stages", "flow: x\nversion: 1\nstages: []\nsteps:\n  - id: a\n", "reserved_key"},
		{"reserved node", "flow: x\nversion: 1\nsteps:\n  - id: a\n    uses: flo/propose@1\n", "reserved_key"},
		{"kind and uses", "flow: x\nversion: 1\nsteps:\n  - id: a\n    kind: exec\n    uses: a/b@1\n", "mutually exclusive"},
		{"uses on block", "flow: x\nversion: 1\nsteps:\n  - id: a\n    repeat: 2\n    uses: a/b@1\n    steps: [{id: b}]\n", "belong on task steps"},
		{"undeclared state", "flow: x\nversion: 1\nsteps:\n  - id: a\n    title: \"{{state.seen}}\"\n", "undeclared state key"},
		{"reserved var trigger", "flow: x\nversion: 1\nvars:\n  trigger: {default: 1}\nsteps:\n  - id: a\n", "reserved"},
		{"bad var type", "flow: x\nversion: 1\nvars:\n  n: {type: bigint}\nsteps:\n  - id: a\n", "unknown type"},
		{"check needs contract", "flow: x\nversion: 1\nsteps:\n  - id: a\n    check: {eva_url: u}\n", "check.contract"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseFlowString(t, tc.yaml)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestParseFlow_AcceptsEngineNamespaces(t *testing.T) {
	_, err := parseFlowString(t, `
flow: x
version: 1
state: {seen: {default: []}}
steps:
  - id: a
    uses: gh/list@1
    for_each: "{{ vars.items }}"
    with: {since: "{{ state.seen }}", who: "{{ trigger.kind }}", one: "{{ item.name }}", nested: {k: ["{{ index }}"]}}
`)
	if err == nil || !strings.Contains(err.Error(), "vars") {
		t.Fatalf("want the undeclared vars.items rejected, got %v", err)
	}
	_, err = parseFlowString(t, `
flow: x
version: 1
vars: {items: {type: list, default: []}}
state: {seen: {default: []}}
steps:
  - id: a
    uses: gh/list@1
    for_each: "{{ vars.items }}"
    with: {since: "{{ state.seen }}", who: "{{ trigger.kind }}", one: "{{ item.name }}", nested: {k: ["{{ index }}"]}}
`)
	if err != nil {
		t.Fatalf("engine namespaces rejected: %v", err)
	}
}

func TestRender_LeavesEngineNamespacesVerbatim(t *testing.T) {
	got, err := Render("{{trigger.kind}} {{state.seen}} {{item}} {{results.a.b}} {{x}}", Scope{Vars: map[string]any{"x": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "{{trigger.kind}} {{state.seen}} {{item}} {{results.a.b}} 1"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRenderStep_WalksNestedWithWithoutMutatingInput(t *testing.T) {
	in := Step{ID: "a", Uses: "x/y@1", With: map[string]any{"a": map[string]any{"b": []any{"{{n}}"}}}}
	out, err := RenderStep(in, Scope{Vars: map[string]any{"n": "7"}})
	if err != nil {
		t.Fatal(err)
	}
	got := out.With["a"].(map[string]any)["b"].([]any)[0]
	if got != "7" {
		t.Errorf("rendered leaf = %v", got)
	}
	if orig := in.With["a"].(map[string]any)["b"].([]any)[0]; orig != "{{n}}" {
		t.Errorf("input mutated: %v", orig)
	}
}

func TestConditions_CELAreDeferred(t *testing.T) {
	cel := `{{ size(results.plan.pending) > 0 }}`
	if _, legacy, err := classifyCondition(cel); err != nil || legacy {
		t.Errorf("classify(%q) legacy=%v err=%v; want CEL", cel, legacy, err)
	}
	if _, legacy, _ := classifyCondition("results.test.exit_code == 0"); !legacy {
		t.Error("single comparison must stay legacy")
	}
	if _, legacy, _ := classifyCondition("{{ results.test.exit_code == 0 }}"); !legacy {
		t.Error("one placeholder around a comparison must stay legacy")
	}
	if _, legacy, _ := classifyCondition("results.a.x == 1 && results.b.y == 2"); legacy {
		t.Error("a compound expression is CEL")
	}
	_, err := EvalConditionEnv(cel, CondEnv{})
	if !errors.Is(err, ErrNeedsCEL) {
		t.Errorf("eval of CEL = %v; want ErrNeedsCEL", err)
	}
}

func TestNegateExpr(t *testing.T) {
	cases := map[string]string{
		"results.t.exit_code == 0":   "results.t.exit_code != 0",
		"results.t.n >= 3":           "results.t.n < 3",
		"{{ results.t.ok && true }}": "!(results.t.ok && true)",
		"size(results.t.items) > 0":  "!(size(results.t.items) > 0)",
	}
	for in, want := range cases {
		if got := negateExpr(in); got != want {
			t.Errorf("negate(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestRepeatUntilCEL_DerivesNegatedWhen(t *testing.T) {
	f, err := parseFlowString(t, `
flow: x
version: 1
steps:
  - id: loop
    repeat: 3
    until: "{{ results.test.exit_code == 0 && true }}"
    steps:
      - id: test
        kind: exec
        exec: {argv: [go, test]}
`)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := Expand(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Fatalf("expanded %d steps", len(steps))
	}
	if steps[0].When != "" {
		t.Errorf("first iteration guarded: %q", steps[0].When)
	}
	if want := "!(results.test.exit_code == 0 && true)"; strings.Replace(steps[1].When, "loop/1/", "", 1) != want {
		t.Errorf("iteration 2 when = %q", steps[1].When)
	}
}

func TestParseFlow_ImplicitEdgeAccepted(t *testing.T) {
	_, err := parseFlowString(t, "flow: x\nversion: 1\nsteps:\n  - id: a\n  - id: b\n    title: \"{{results.a.stdout}}\"\n")
	if err != nil {
		t.Fatalf("a results reference without depends_on is an implicit edge: %v", err)
	}
}
