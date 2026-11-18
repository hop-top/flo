package render_test

import (
	"reflect"
	"testing"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/render"
	"hop.top/flo/spec/node"
)

func scope() render.Scope {
	return render.Scope{
		In: map[string]any{
			"repo":  "spf13/cobra",
			"repos": []any{"a/b", "c/d"},
			"none":  nil,
			"n":     int64(3),
		},
		Config:  map[string]any{"url": "http://x"},
		Run:     core.RunContext{ID: "run_1", Flow: "f"},
		StepDir: "/tmp/step",
	}
}

func TestArgv(t *testing.T) {
	env, err := render.Shared()
	if err != nil {
		t.Fatal(err)
	}
	got, err := env.Argv([]string{
		"gh", "api", "repos/{{ in.repo }}/sbom", "--out", "{{ run.step_dir }}/out.json",
		"--", "{{ in.repos }}", "{{ in.none }}", "{{ in.n }}", "n={{ in.n }}",
	}, scope().Activation())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gh", "api", "repos/spf13/cobra/sbom", "--out", "/tmp/step/out.json", "--", "a/b", "c/d", "", "3", "n=3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q\nwant   %q", got, want)
	}
}

func TestArgvUnresolved(t *testing.T) {
	env, _ := render.Shared()
	_, err := env.Argv([]string{"{{ in.missing }}"}, scope().Activation())
	if err == nil {
		t.Fatal("want error")
	}
	if e := render.Fail("argv", err); e.Code != core.CodeUnresolvedRef {
		t.Fatalf("code = %s", e.Code)
	}
}

func TestReshapeOut(t *testing.T) {
	env, _ := render.Shared()
	def := node.OutputDef{
		Map: "{{ {'source': out.source, 'releases': out.releases.map(r, {'package': in.repo, 'v': r.version})} }}",
		// out is the mapped value; a field named out stays a field.
		FailWhen: "{{ size(out.releases) == 0 }}",
	}
	parsed := map[string]any{"source": "tags", "releases": []any{map[string]any{"version": "1.0.0"}}}
	v, failed, err := env.Reshape(def, parsed, scope())
	if err != nil || failed {
		t.Fatalf("v=%v failed=%v err=%v", v, failed, err)
	}
	m := v.(map[string]any)
	rel := m["releases"].([]any)[0].(map[string]any)
	if m["source"] != "tags" || rel["package"] != "spf13/cobra" || rel["v"] != "1.0.0" {
		t.Fatalf("v = %v", v)
	}

	_, failed, err = env.Reshape(def, map[string]any{"source": "none", "releases": []any{}}, scope())
	if err != nil || !failed {
		t.Fatalf("failed=%v err=%v", failed, err)
	}
}

func TestReshapeOutIdentifierRules(t *testing.T) {
	env, _ := render.Shared()
	// a string literal and a field selection named out are not rewritten
	def := node.OutputDef{Map: "{{ {'out': out.out, 'text': 'out of {x}'} }}"}
	v, _, err := env.Reshape(def, map[string]any{"out": int64(7)}, scope())
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if m["out"] != int64(7) || m["text"] != "out of {x}" {
		t.Fatalf("v = %v", m)
	}
}

func TestReshapeFailWhenNotBool(t *testing.T) {
	env, _ := render.Shared()
	_, _, err := env.Reshape(node.OutputDef{FailWhen: "{{ out }}"}, "x", scope())
	if err == nil || err.Code != core.CodeEvalError {
		t.Fatalf("err = %v", err)
	}
}

func TestItem(t *testing.T) {
	env, _ := render.Shared()
	f, err := env.Item("{{ item.a + '@' + string(index) }}")
	if err != nil {
		t.Fatal(err)
	}
	v, verr := f(map[string]any{"a": "x"}, 2)
	if verr != nil || v != "x@2" {
		t.Fatalf("v=%v err=%v", v, verr)
	}
	g, err := env.Item("item.n > 1")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := g(map[string]any{"n": int64(2)}, 0); v != true {
		t.Fatalf("v = %v", v)
	}
}

func TestInKeywordAndNamespace(t *testing.T) {
	env, _ := render.Shared()
	// `in` is a CEL keyword: the namespace is rewritten, the operator is not.
	v, err := env.Value("{{ 'a/b' in in.repos && !(in.repo in ['x']) }}", scope().Activation())
	if err != nil || v != true {
		t.Fatalf("v=%v err=%v", v, err)
	}
	v, err = env.Value(map[string]any{"k": "{{ in.n }}", "l": []any{"{{ in.repo }}", 1}}, scope().Activation())
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if m["k"] != int64(3) || m["l"].([]any)[0] != "spf13/cobra" || m["l"].([]any)[1] != 1 {
		t.Fatalf("v = %v", m)
	}
}
