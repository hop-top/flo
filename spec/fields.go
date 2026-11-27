package spec

import (
	"fmt"
	"sort"
)

// Field is one templatable string of a step: its path in the document
// (title, exec.argv[0], with.items, outputs.state.seen, ...) and its value.
type Field struct {
	Path  string
	Value string
}

// StepFields lists every templatable string of s in a stable order, the
// same set WalkStepStrings visits, each with its document path. It reads
// s and never mutates it; nested steps are not included.
func StepFields(s *Step) []Field {
	out := []Field{
		{"title", s.Title}, {"description", s.Description}, {"assignee", s.Assignee},
		{"due", s.Due.Raw}, {"when", s.When}, {"until", s.Until}, {"agent", s.Agent},
	}
	if s.Exec != nil {
		for i, a := range s.Exec.Argv {
			out = append(out, Field{fmt.Sprintf("exec.argv[%d]", i), a})
		}
		out = append(out, Field{"exec.cwd", s.Exec.Cwd})
		keys := make([]string, 0, len(s.Exec.Env))
		for k := range s.Exec.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, Field{"exec.env." + k, s.Exec.Env[k]})
		}
	}
	if s.Human != nil {
		out = append(out, Field{"human.assignee", s.Human.Assignee})
	}
	if s.Check != nil {
		out = append(out, Field{"check.contract", s.Check.Contract})
	}
	out = appendAnyFields(out, "with", s.With)
	out = appendAnyFields(out, "outputs", s.Outputs)
	out = appendExtFields(out, s.Ext)
	out = append(out, Field{"for_each", s.ForEach})
	var kept []Field
	for _, f := range out {
		if f.Value != "" {
			kept = append(kept, f)
		}
	}
	return kept
}

// appendExtFields lists the string leaves of the extension map under
// their own key paths (x-acme.tags[0]).
func appendExtFields(out []Field, m map[string]any) []Field {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = appendAnyField(out, k, m[k])
	}
	return out
}

func appendAnyFields(out []Field, prefix string, m map[string]any) []Field {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = appendAnyField(out, prefix+"."+k, m[k])
	}
	return out
}

func appendAnyField(out []Field, path string, v any) []Field {
	switch t := v.(type) {
	case string:
		out = append(out, Field{path, t})
	case map[string]any:
		out = appendAnyFields(out, path, t)
	case []any:
		for i, e := range t {
			out = appendAnyField(out, fmt.Sprintf("%s[%d]", path, i), e)
		}
	}
	return out
}
