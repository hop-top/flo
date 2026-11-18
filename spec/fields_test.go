package spec

import "testing"

// StepFields must list exactly the strings walkStepStrings visits, or the
// two lists of "where a template may appear" drift apart.
func TestStepFields_MatchesWalk(t *testing.T) {
	s := Step{
		ID: "a", Title: "t", Description: "d", Assignee: "u", When: "w", Until: "x", Agent: "g",
		Due:     DueSpec{Raw: "+1d"},
		Exec:    &ExecSpec{Argv: []string{"a", "b"}, Cwd: "c", Env: map[string]string{"K": "v", "J": "u"}},
		Human:   &HumanSpec{Assignee: "h"},
		Check:   &Check{Contract: "k"},
		With:    map[string]any{"p": "q", "n": map[string]any{"m": "r", "l": []any{"s", 1}}},
		Outputs: map[string]any{"state": map[string]any{"z": "y"}},
		ForEach: "f",
	}
	want := 0
	c := s
	walkStepStrings(&c, func(p *string) {
		if *p != "" {
			want++
		}
	})
	got := StepFields(&s)
	if len(got) != want {
		t.Fatalf("StepFields = %d fields (%v), walkStepStrings = %d", len(got), got, want)
	}
	paths := map[string]string{}
	for _, f := range got {
		paths[f.Path] = f.Value
	}
	for path, v := range map[string]string{"exec.argv[1]": "b", "exec.env.K": "v", "with.n.l[0]": "s", "outputs.state.z": "y", "for_each": "f"} {
		if paths[path] != v {
			t.Errorf("field %s = %q, want %q", path, paths[path], v)
		}
	}
}

func TestIsLegacyCondition(t *testing.T) {
	for expr, want := range map[string]bool{
		"results.a.exit_code == 0":       true,
		"{{ vars.x != 'y' }}":            true,
		"results.sec-scan.exit_code > 1": true,
		"size(results.a.x) > 0":          false,
		"a == 1 && b == 2":               false,
		"!(a == 1)":                      false,
	} {
		if got := IsLegacyCondition(expr); got != want {
			t.Errorf("IsLegacyCondition(%q) = %v, want %v", expr, got, want)
		}
	}
}
