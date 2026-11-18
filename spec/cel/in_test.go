package cel

import (
	"slices"
	"strings"
	"testing"

	"hop.top/flo/spec"
)

func inAct() Activation {
	return Activation{"in": map[string]any{"x": int64(3), "a-b": "dash", "l": []any{"p"}}, "vars": map[string]any{}}
}

func TestInNamespace(t *testing.T) {
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		src  string
		want any
	}{
		{"in.x", int64(3)},
		{"in.x > 1", true},
		{"has(in.x)", true},
		{"has(in.nope)", false},
		{"in['a-b']", "dash"},
		{"in.l.map(i, i + '!')", []any{"p!"}},
		{"!has(in.nope) && in.x == 3", true},
		{`"in" in ["in"]`, true},
		{`'in.x' == 'in.x'`, true},
		{`"p" in in.l`, true},
		{`in.x in [3]`, true},
		{"[in.x][0]", int64(3)},
	}
	for _, c := range cases {
		x, err := env.Compile(c.src)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		got, err := x.Eval(inAct())
		if err != nil {
			t.Errorf("%s: eval: %v", c.src, err)
			continue
		}
		if !equal(got, c.want) {
			t.Errorf("%s = %#v, want %#v", c.src, got, c.want)
		}
	}
}

func equal(a, b any) bool {
	if la, ok := a.([]any); ok {
		lb, ok := b.([]any)
		return ok && slices.Equal(la, lb)
	}
	return a == b
}

func TestInTemplatesConditionsValues(t *testing.T) {
	env, _ := NewEnv()
	tp, err := env.CompileTemplate("n={{ in.x }};{{ in['a-b'] }}")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := tp.Render(inAct()); got != "n=3;dash" {
		t.Errorf("template = %v", got)
	}
	c, err := env.CompileCondition("{{ in.x > 1 }}")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := c.Eval(inAct()); err != nil || !ok {
		t.Errorf("condition = %v, %v", ok, err)
	}
	got, err := env.RenderValue(map[string]any{"k": "{{ in.x }}", "l": []any{"{{ in.l }}"}}, inAct())
	if err != nil || got.(map[string]any)["k"] != int64(3) {
		t.Errorf("RenderValue = %#v, %v", got, err)
	}
	f := &spec.Flow{Vars: map[string]spec.VarDef{"a": {}, "b": {}}}
	fe, err := EnvForFlow(f)
	if err != nil {
		t.Fatal(err)
	}
	x, err := fe.Compile("a in [in.x]")
	if err != nil {
		t.Fatalf("a in b operator must survive: %v", err)
	}
	_ = x
}

func TestInRefsReportIn(t *testing.T) {
	env, _ := NewEnv()
	x, err := env.Compile("in.x + 1 > 0 && results.a.y == 1")
	if err != nil {
		t.Fatal(err)
	}
	r := x.Refs()
	if !slices.Contains(r.Namespaces, "in") || slices.Contains(r.Namespaces, inAlias) || !slices.Equal(r.Results, []string{"a"}) {
		t.Errorf("refs = %+v", r)
	}
	f := &spec.Step{ID: "s", With: map[string]any{"k": "{{ results.up.v }} {{ in.x }}"}}
	if deps := env.ImplicitDeps(f); !slices.Equal(deps, []string{"up"}) {
		t.Errorf("deps = %v", deps)
	}
}

func TestInErrorsPointAtOriginalText(t *testing.T) {
	env, _ := NewEnv()
	_, err := env.CompileTemplate("ab {{ in.x + }}")
	es, ok := err.(Errors)
	if !ok || len(es) == 0 {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(es[0].Error(), inAlias) {
		t.Errorf("alias leaked: %v", es[0])
	}
	_, err = env.Compile("in.x +\n  in.y.")
	if err == nil {
		t.Fatal("want a syntax error")
	}
	if e := err.(Errors)[0]; e.Line != 2 {
		t.Errorf("line = %d, want 2 (positions map back to the original text)", e.Line)
	}
	_, err = env.Compile("in.x + 'a'")
	if err != nil {
		t.Fatalf("dyn field arithmetic compiles: %v", err)
	}
	x, _ := env.Compile("in.x")
	if _, err := x.Eval(Activation{}); err == nil || strings.Contains(err.Error(), inAlias) {
		t.Errorf("missing in must be an error without the alias: %v", err)
	}
}

func TestInRewriteUnit(t *testing.T) {
	cases := map[string]string{
		"in.x":             "in__.x",
		"a in b":           "a in b",
		`"in.x" in m`:      `"in.x" in m`,
		"x.in.y":           "x.in.y",
		"f(in.x, in['k'])": "f(in__.x, in__['k'])",
		"x in in.l":        "x in in__.l",
		"a in [1]":         "a in [1]",
		"m in{'a': 1}":     "m in{'a': 1}",
		"!in.x":            "!in__.x",
		"a ? in.x : in.y":  "a ? in__.x : in__.y",
		"(a) in [in.x]":    "(a) in [in__.x]",
	}
	for src, want := range cases {
		if got, _ := normalizeRefs(src, nil); got != want {
			t.Errorf("%q -> %q, want %q", src, got, want)
		}
	}
}

// A bare `in` is not the namespace: it stays a syntax error.
func TestBareInStaysAnError(t *testing.T) {
	env, _ := NewEnv()
	for _, src := range []string{"in", "(in)", "in + 1"} {
		if _, err := env.Compile(src); err == nil {
			t.Errorf("%q compiled", src)
		}
	}
}
