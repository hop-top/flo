package cel_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"hop.top/flo/spec"
	"hop.top/flo/spec/cel"
)

func newEnv(t *testing.T) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func act() cel.Activation {
	return cel.Activation{
		"results": map[string]any{
			"a":        map[string]any{"exit_code": int64(1), "items": []any{"x", "y"}, "n": int64(3)},
			"sec-scan": map[string]any{"stdout": "ok"},
			"sec/scan": map[string]any{"stdout": "nested"},
			"deps":     []any{map[string]any{"items": []any{"a", "b"}}, map[string]any{"items": []any{"c"}}},
		},
		"state": map[string]any{"cursor": "c1"}, "trigger": map[string]any{}, "config": map[string]any{},
		"run": map[string]any{"id": "r1"}, "item": "it", "index": int64(2),
		"vars": map[string]any{"repo": "r"}, "subject": map[string]any{}, "steps": map[string]any{},
		"in": map[string]any{}, "result": map[string]any{}, "decisions": map[string]any{},
	}
}

// examples.md flows all pass spec's validator and then compile clean.
func TestExamples_CompileClean(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "spec", "examples.md"))
	if err != nil {
		t.Fatal(err)
	}
	fence := regexp.MustCompile("(?s)```yaml\n(flow: .*?)```")
	n := 0
	for _, m := range fence.FindAllStringSubmatch(string(data), -1) {
		name, _, _ := strings.Cut(strings.TrimPrefix(m[1], "flow: "), "\n")
		n++
		t.Run(name, func(t *testing.T) {
			if _, err := cel.ParseFlow(strings.NewReader(m[1]), name+".yaml"); err != nil {
				t.Fatalf("%v", err)
			}
		})
	}
	if n != 3 {
		t.Errorf("examples.md has %d flows, want 3", n)
	}
}

func parse(t *testing.T, doc string) *spec.Flow {
	t.Helper()
	f, err := spec.ParseFlow(strings.NewReader(doc), "t.yaml")
	if err != nil {
		t.Fatalf("spec rejects the fixture: %v", err)
	}
	return f
}

// wantErr finds one error of the given code at step/field and position.
func wantErr(t *testing.T, err error, code, step, field string, line, col int) {
	t.Helper()
	var errs cel.Errors
	if !errors.As(err, &errs) {
		t.Fatalf("err = %v, want cel.Errors", err)
	}
	for _, e := range errs {
		if e.Code == code && e.Step == step && e.Field == field && e.Line == line && e.Col == col {
			return
		}
	}
	t.Errorf("no %s at step %q field %q %d:%d in:\n%v", code, step, field, line, col, err)
}

const head = "flow: t\nversion: 1.0.0\nvars:\n  n: {type: integer, default: 1}\n  s: {default: x}\nstate:\n  cursor: {default: ''}\nsteps:\n"

func TestValidate_Negative(t *testing.T) {
	tests := []struct {
		name, steps, code, step, field string
		line, col                      int
	}{
		{"undeclared ref in when", "  - id: a\n    when: \"nope.x > 1\"\n", cel.CodeUndeclaredRef, "a", "when", 1, 1},
		{"bad overload", "  - id: a\n    when: \"'a' + 1 == 2\"\n", cel.CodeType, "a", "when", 1, 5},
		{"syntax error", "  - id: a\n    when: \"size(results.b.x > 1 &&\"\n", cel.CodeSyntax, "a", "when", 1, 24},
		{"unknown function", "  - id: a\n    title: \"x {{ frobnicate(1) }}\"\n", cel.CodeUnknownFunc, "a", "title", 1, 16},
		{"typed var", "  - id: a\n    title: \"{{ n + 'x' }}\"\n", cel.CodeType, "a", "title", 1, 6},
		{"index is int", "  - id: a\n    title: \"{{ index > 'a' }}\"\n", cel.CodeType, "a", "title", 1, 10},
		{"not boolean", "  - id: a\n    when: \"size(s) + 1\"\n", cel.CodeNotBoolean, "a", "when", 1, 1},
		{"multi-line position", "  - id: a\n    description: |-\n      line one\n      {{ s +\n        nope }}\n", cel.CodeUndeclaredRef, "a", "description", 3, 3},
		{"unterminated", "  - id: a\n    title: \"ab {{ s\"\n", cel.CodeUnterminated, "a", "title", 1, 4},
		{"unknown state key", "  - id: a\n    title: \"{{ size(state.nope) }}\"\n", cel.CodeUnknownState, "a", "title", 0, 0},
		{"unknown step", "  - id: a\n    title: \"{{ size(results.zz.x) }}\"\n", cel.CodeUnknownStep, "a", "title", 0, 0},
		{"own results", "  - id: a\n    when: \"results.a.x == 1 || false\"\n", cel.CodeCycle, "a", "when", 0, 0},
		{"result out of scope", "  - id: a\n    title: \"{{ result.x }}\"\n", cel.CodeOutOfScope, "a", "title", 0, 0},
		{"item out of scope", "  - id: a\n    title: \"{{ item.x }}\"\n", cel.CodeOutOfScope, "a", "title", 0, 0},
		{"for_each not a list", "  - id: a\n    for_each: \"{{ 'abc' }}\"\n", cel.CodeNotList, "a", "for_each", 1, 1},
		{"nested with path", "  - id: a\n    uses: x/y@1\n    with: {k: {l: [\"{{ size(nope) }}\"]}}\n", cel.CodeUndeclaredRef, "a", "with.k.l[0]", 1, 9},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := parse(t, head+tc.steps)
			wantErr(t, cel.Validate(f), tc.code, tc.step, tc.field, tc.line, tc.col)
		})
	}
}

func TestValidate_CELDependencyCycle(t *testing.T) {
	f := parse(t, head+"  - id: a\n    when: \"size(results.b.x) > 0 || false\"\n  - id: b\n    when: \"size(results.a.x) > 0 || false\"\n")
	wantErr(t, cel.Validate(f), cel.CodeCycle, "b", "", 0, 0)
}

func TestValidate_Accepts(t *testing.T) {
	f := parse(t, head+`  - id: a
    kind: exec
    exec: {argv: [echo, "{{ s }}", "{{ vars.s }}"]}
  - id: sec-scan
    kind: exec
    exec: {argv: [true]}
  - id: b
    depends_on: [a]
    title: "{{ results.sec-scan.stdout }} {{ results.a.stdout }} {{ results['sec-scan'].stdout }}"
    when: "has(results.a) && size(results.a.stdout) > 0"
    outputs: {state: {cursor: "{{ result.cursor + state.cursor }}"}}
  - id: c
    uses: x/y@1
    for_each: "{{ [[1],[2]].flatten() }}"
    with: {k: "{{ item + index }}"}
`)
	if err := cel.Validate(f); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyConditionsUnchanged(t *testing.T) {
	env := newEnv(t)
	c, err := env.CompileCondition("results.a.exit_code == '1'")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Legacy() || c.Expr() != nil {
		t.Fatalf("legacy = %v, expr = %v; want the shim path", c.Legacy(), c.Expr())
	}
	// loose equality: 1 equals '1'
	if ok, err := c.Eval(act()); err != nil || !ok {
		t.Errorf("legacy Eval = %v, %v; want true", ok, err)
	}
	// legacy placeholders and hyphenated ids stay on the shim
	for _, expr := range []string{"{{ results.sec-scan.stdout != 'bad' }}", "vars.repo == r", "results.a.n >= 3"} {
		c, err := env.CompileCondition(expr)
		if err != nil || !c.Legacy() {
			t.Fatalf("%q: legacy = %v err = %v", expr, c != nil && c.Legacy(), err)
		}
		if ok, err := c.Eval(act()); err != nil || !ok {
			t.Errorf("%q = %v, %v; want true", expr, ok, err)
		}
	}
	// the same comparison as CEL is strictly typed: 1 != '1'
	c, err = env.CompileCondition("results.a.exit_code == '1' || false")
	if err != nil || c.Legacy() {
		t.Fatalf("CEL form: %v legacy=%v", err, c != nil && c.Legacy())
	}
	if ok, _ := c.Eval(act()); ok {
		t.Error("CEL 1 == '1' is true; want strict false")
	}
}

func TestCondition_CEL(t *testing.T) {
	env := newEnv(t)
	for expr, want := range map[string]bool{
		"size(results.a.items) > 1":                                true,
		"{{ results.deps.map(d, d.items).flatten().size() == 3 }}": true,
		"has(results.nope) || index == 2":                          true,
		"!(results.a.n > 2 && index == 2)":                         false,
		"results['sec-scan'].stdout == 'ok'":                       true,
		"results.sec/scan.stdout == 'nested' && true":              true,
		"'a,b'.split(',').size() == 2":                             true,
		"":                                                         true,
	} {
		c, err := env.CompileCondition(expr)
		if err != nil {
			t.Errorf("%q: %v", expr, err)
			continue
		}
		got, err := c.Eval(act())
		if err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v", expr, got, err, want)
		}
	}
}

func TestHyphenIDsNeedKnownSteps(t *testing.T) {
	f := parse(t, head+"  - id: sec-scan\n    kind: exec\n    exec: {argv: [x]}\n  - id: b\n    when: \"results.sec-scan.stdout == 'ok' && true\"\n")
	env, err := cel.EnvForFlow(f)
	if err != nil {
		t.Fatal(err)
	}
	c, err := env.CompileCondition(f.Steps[1].When)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := c.Eval(act()); err != nil || !ok {
		t.Errorf("= %v, %v; want true", ok, err)
	}
	if got := env.ImplicitDeps(&f.Steps[1]); !reflect.DeepEqual(got, []string{"sec-scan"}) {
		t.Errorf("ImplicitDeps = %v", got)
	}
}

func TestTemplate_Render(t *testing.T) {
	f := parse(t, head+"  - id: a\n    kind: exec\n    exec: {argv: [x]}\n")
	env, _ := cel.EnvForFlow(f)
	a := cel.BindVars(act(), map[string]any{"n": int64(7), "s": "str"})
	tests := []struct {
		in   string
		want any
	}{
		{"{{ results.a.items }}", []any{"x", "y"}},
		{"  {{ results.a.n }}\n", int64(3)},
		{"{{ index + 1 }}", int64(3)},
		{"{{ null }}", nil},
		{"{{ {'a': {'b': 1}} }}", map[string]any{"a": map[string]any{"b": int64(1)}}},
		{"{{ '}}' }}", "}}"},
		{"{{ results.deps.map(d, d.items).flatten() }}", []any{"a", "b", "c"}},
		{"pre {{ results.a.items }} post", "pre x,y post"},
		{"n={{ n }} s={{ s }}", "n=7 s=str"},
		{"{{ results.a }}/{{ true }}", `{"exit_code":1,"items":["x","y"],"n":3}/true`},
		{"{{ [['a'], ['b']].flatten().join('-') }}", "a-b"},
		{"{{ s.upperAscii() }}", "STR"},
		{"no placeholders", "no placeholders"},
		{"{{ results.sec/scan.stdout }}", "nested"},
		{"{{ run.id }}-{{ vars.s }}", "r1-str"},
	}
	for _, tc := range tests {
		tpl, err := env.CompileTemplate(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		got, err := tpl.Render(a)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q = %#v, %v; want %#v", tc.in, got, err, tc.want)
		}
	}
}

func TestTemplate_Unresolved(t *testing.T) {
	env := newEnv(t)
	tpl, err := env.CompileTemplate("a {{ results.a.missing }}")
	if err != nil {
		t.Fatal(err)
	}
	_, err = tpl.Render(act())
	var ce *cel.Error
	if !errors.As(err, &ce) || ce.Code != cel.CodeUnresolved {
		t.Errorf("err = %v, want %s", err, cel.CodeUnresolved)
	}
}

func TestRefs(t *testing.T) {
	env := newEnv(t)
	x, err := env.Compile(`results.a.x + results['b'].y > 0 && state.k == 1 && results.c.map(r, r.d).size() > index && has(results.e.f)`)
	if err != nil {
		t.Fatal(err)
	}
	r := x.Refs()
	if !reflect.DeepEqual(r.Results, []string{"a", "b", "c", "e"}) || !reflect.DeepEqual(r.State, []string{"k"}) {
		t.Errorf("refs = %+v", r)
	}
}

func TestEnv_Functions(t *testing.T) {
	env := newEnv(t)
	for _, expr := range []string{"[[1, 2], [3]].flatten().size() == 3", "['a', 'b'].join(',') == 'a,b'", "[3, 1, 2].sort()[0] == 1"} {
		x, err := env.Compile(expr)
		if err != nil {
			t.Errorf("%s: %v", expr, err)
			continue
		}
		if v, err := x.Eval(cel.Activation{}); err != nil || v != true {
			t.Errorf("%s = %v, %v", expr, v, err)
		}
	}
}

func TestRepeatUntil_DerivedWhenCompiles(t *testing.T) {
	f := parse(t, head+`  - id: loop
    repeat: 2
    until: "results.verify.exit_code == 0 && size(results.verify.stdout) > 0"
    steps:
      - id: fix
        kind: exec
        exec: {argv: [fix]}
      - id: verify
        kind: exec
        exec: {argv: [verify]}
        depends_on: [fix]
`)
	if err := cel.Validate(f); err != nil {
		t.Fatalf("unexpanded: %v", err)
	}
	steps, err := spec.Expand(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	var derived string
	for _, s := range steps {
		if strings.HasPrefix(s.When, "!(") {
			derived = s.When
		}
	}
	if derived == "" {
		t.Fatalf("no negated derived when in %+v", steps)
	}
	expanded := *f
	expanded.Steps = steps
	if err := cel.Validate(&expanded); err != nil {
		t.Fatalf("expanded %q: %v", derived, err)
	}
}

func TestParseFlow_WrapsSpec(t *testing.T) {
	if _, err := cel.ParseFlow(strings.NewReader("flow: Bad Name\n"), "x.yaml"); err == nil {
		t.Error("spec error not returned")
	}
	_, err := cel.ParseFlow(strings.NewReader(head+"  - id: a\n    when: \"nope.x > 1\"\n"), "x.yaml")
	if err == nil || !strings.Contains(err.Error(), "x.yaml") || !strings.Contains(err.Error(), "step a: when: 1:1") {
		t.Errorf("err = %v", err)
	}
}
