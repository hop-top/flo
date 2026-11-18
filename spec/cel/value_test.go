package cel_test

import (
	"errors"
	"reflect"
	"testing"

	"hop.top/flo/spec/cel"
)

func TestRenderValue(t *testing.T) {
	env := newEnv(t)
	tests := []struct {
		name string
		in   any
		want any
	}{
		{"plain scalars untouched", map[string]any{"a": int64(1), "b": true, "c": nil, "d": 1.5}, map[string]any{"a": int64(1), "b": true, "c": nil, "d": 1.5}},
		{"literal string", "hello", "hello"},
		{"whole placeholder keeps list", "{{ results.a.items }}", []any{"x", "y"}},
		{"whole placeholder keeps number", "{{ results.a.n }}", int64(3)},
		{"whole placeholder keeps null", "{{ null }}", nil},
		{"whole placeholder keeps map", "{{ {'k': results.a.n} }}", map[string]any{"k": int64(3)}},
		{"whole placeholder keeps bool", "{{ results.a.exit_code == 1 }}", true},
		{"text around renders string", "n={{ results.a.n }}", "n=3"},
		{"text around list joins", "items:{{ results.a.items }}", "items:x,y"},
		{"text around map is json", "m={{ {'k': 1} }}", `m={"k":1}`},
		{"nested map", map[string]any{"x": map[string]any{"y": "{{ index }}", "z": []any{"{{ item }}", "k-{{ item }}"}}},
			map[string]any{"x": map[string]any{"y": int64(2), "z": []any{"it", "k-it"}}}},
		{"list of any", []any{"{{ results.a.items }}", int64(7)}, []any{[]any{"x", "y"}, int64(7)}},
		{"typed string slice", []string{"a", "{{ item }}"}, []any{"a", "it"}},
		{"typed string map", map[string]string{"A": "{{ item }}"}, map[string]any{"A": "it"}},
		{"keys are not templated", map[string]any{"{{ item }}": "v"}, map[string]any{"{{ item }}": "v"}},
		{"empty containers", map[string]any{"l": []any{}, "m": map[string]any{}}, map[string]any{"l": []any{}, "m": map[string]any{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := env.RenderValue(tt.in, act())
			if err != nil {
				t.Fatalf("RenderValue: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %#v\nwant %#v", got, tt.want)
			}
		})
	}
}

func TestRenderValueDoesNotAliasInput(t *testing.T) {
	env := newEnv(t)
	in := map[string]any{"a": []any{"{{ item }}"}}
	got, err := env.RenderValue(in, act())
	if err != nil {
		t.Fatal(err)
	}
	got.(map[string]any)["a"].([]any)[0] = "changed"
	if in["a"].([]any)[0] != "{{ item }}" {
		t.Error("rendering must not mutate the template tree")
	}
}

func TestRenderValueUnresolvedKeepsCodeAndPath(t *testing.T) {
	env := newEnv(t)
	in := map[string]any{"repos": []any{"ok", "{{ results.missing.items }}"}}
	_, err := env.RenderValue(in, act())
	var ce *cel.Error
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *cel.Error", err)
	}
	if ce.Code != cel.CodeUnresolved || ce.Field != "repos[1]" {
		t.Errorf("error = %+v, want unresolved_ref at repos[1]", ce)
	}
}

func TestCompileValueReportsEveryLeaf(t *testing.T) {
	env := newEnv(t)
	_, err := env.CompileValue("with", map[string]any{
		"a": "{{ nope( }}",
		"b": []any{"{{ 1 + }}"},
		"c": "fine",
	})
	var es cel.Errors
	if !errors.As(err, &es) || len(es) != 2 {
		t.Fatalf("err = %v, want 2 errors", err)
	}
	if es[0].Field != "with.a" || es[1].Field != "with.b[0]" {
		t.Errorf("fields = %q, %q", es[0].Field, es[1].Field)
	}
}

func TestCompiledValueRendersPerElement(t *testing.T) {
	env := newEnv(t)
	v, err := env.CompileValue("with", map[string]any{"package": "{{ item.name }}", "i": "{{ index }}"})
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"a", "b"} {
		a := act()
		a["item"] = map[string]any{"name": name}
		a["index"] = int64(i)
		got, err := v.Render(a)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"package": name, "i": int64(i)}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("element %d: %v, want %v", i, got, want)
		}
	}
}
