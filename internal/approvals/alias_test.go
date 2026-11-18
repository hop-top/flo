package approvals

import "testing"

func TestAliasIn(t *testing.T) {
	for in, want := range map[string]string{
		"plain in.text, no placeholder":                "plain in.text, no placeholder",
		"{{ in.context.run }}":                         "{{ result.context.run }}",
		"a {{ in.x }} b {{ in.y }} in.z":               "a {{ result.x }} b {{ result.y }} in.z",
		"{{ in.level == 'auto' ? 'in.x' : \"in.y\" }}": "{{ result.level == 'auto' ? 'in.x' : \"in.y\" }}",
		"{{ x in y }}":                                 "{{ x in y }}",
		"{{ a.in.b }}":                                 "{{ a.in.b }}",
		"{{ in .z }}":                                  "{{ result .z }}",
		"{{ unterminated":                              "{{ unterminated",
	} {
		if got := aliasIn(in); got != want {
			t.Errorf("aliasIn(%q) = %q, want %q", in, got, want)
		}
	}
	v := aliasInValue(map[string]any{"a": []any{"{{ in.x }}", 3}, "b": "{{ in.y }}"}).(map[string]any)
	if v["b"] != "{{ result.y }}" || v["a"].([]any)[0] != "{{ result.x }}" || v["a"].([]any)[1] != 3 {
		t.Errorf("aliasInValue = %v", v)
	}
}
