package spec

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// PlaceholderRe matches {{ref}} where ref is a bare var name or a
// dotted path whose later segments may contain the "/" and "-" that
// expanded step ids carry: {{pr}}, {{vars.pr}}, {{subject.title}},
// {{run.iteration}}, {{steps.planning.due}}, {{results.sec/scan.stdout}}.
var PlaceholderRe = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z0-9_/-]+)*)\s*\}\}`)

// stepRefRe matches a results.<id>. or steps.<id>. reference wherever
// it appears — inside a placeholder or bare in a condition — so expansion
// can rewrite ids in one pass.
var stepRefRe = regexp.MustCompile(`\b(results|steps)\.([A-Za-z0-9_/-]+)\.`)

// Scope is the create-time template environment. Results are not in it:
// {{results.*}} stays verbatim for the executor to resolve at dispatch.
type Scope struct {
	Vars    map[string]any
	Subject map[string]any
	Run     map[string]any
	Steps   map[string]map[string]any
}

// Render substitutes every placeholder in s from scope. Unknown references
// are errors so a typo never ships as literal braces in a task.
func Render(s string, scope Scope) (string, error) {
	var firstErr error
	out := PlaceholderRe.ReplaceAllStringFunc(s, func(match string) string {
		ref := PlaceholderRe.FindStringSubmatch(match)[1]
		if head, _, _ := strings.Cut(ref, "."); head == nsResults || engineNamespaces[head] {
			return match // bound at dispatch or by the engine
		}
		v, err := scope.lookup(ref)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return match
		}
		return TemplateValue(v)
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

func (sc Scope) lookup(ref string) (any, error) {
	ns, rest, dotted := strings.Cut(ref, ".")
	if !dotted {
		return scopeKey(sc.Vars, ref, "var")
	}
	switch ns {
	case nsVars:
		return scopeKey(sc.Vars, rest, "var")
	case nsSubject:
		return scopeKey(sc.Subject, rest, "subject field")
	case nsRun:
		return scopeKey(sc.Run, rest, "run field")
	case nsSteps:
		id, field, ok := strings.Cut(rest, ".")
		if !ok {
			return nil, fmt.Errorf("{{%s}}: want steps.<id>.<field>", ref)
		}
		fields, found := sc.Steps[id]
		if !found {
			return nil, fmt.Errorf("{{%s}}: step %q is not rendered yet or does not exist", ref, id)
		}
		return scopeKey(fields, field, "step field")
	}
	return nil, fmt.Errorf("{{%s}}: unknown namespace %q", ref, ns)
}

func scopeKey(m map[string]any, key, what string) (any, error) {
	v, ok := m[key]
	if !ok {
		return nil, fmt.Errorf("unknown %s %q", what, key)
	}
	return v, nil
}

// TemplateValue renders a bound value the way a task field expects it:
// lists join with commas, everything else prints plainly.
func TemplateValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []string:
		return strings.Join(t, ",")
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = fmt.Sprint(e)
		}
		return strings.Join(parts, ",")
	}
	return fmt.Sprint(v)
}

// RenderStep renders every templatable field of step against scope and
// returns the result as a copy; the input is never mutated.
func RenderStep(step Step, scope Scope) (Step, error) {
	out := CopyStep(step)
	var firstErr error
	WalkStepStrings(&out, func(s *string) {
		r, err := Render(*s, scope)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		*s = r
	})
	if firstErr != nil {
		return Step{}, fmt.Errorf("step %s: %w", step.ID, firstErr)
	}
	return out, nil
}

// CopyStep deep-copies the parts of a step that rendering or expansion
// mutate. Nested Steps share their backing array; expansion never edits
// them in place.
func CopyStep(s Step) Step {
	out := s
	out.DependsOn = slices.Clone(s.DependsOn)
	if s.Exec != nil {
		e := *s.Exec
		e.Argv = slices.Clone(s.Exec.Argv)
		e.Env = maps.Clone(s.Exec.Env)
		out.Exec = &e
	}
	if s.Human != nil {
		h := *s.Human
		out.Human = &h
	}
	if s.Retry != nil {
		r := *s.Retry
		out.Retry = &r
	}
	if s.Check != nil {
		g := *s.Check
		out.Check = &g
	}
	out.With = cloneAnyMap(s.With)
	out.Outputs = cloneAnyMap(s.Outputs)
	return out
}

// WalkStepStrings visits every templatable string of a step in place.
// It is the single list of "fields a template may appear in", shared by
// rendering, validation and expansion-time rewriting.
func WalkStepStrings(s *Step, fn func(*string)) {
	for _, p := range []*string{&s.Title, &s.Description, &s.Assignee, &s.Due.Raw, &s.When, &s.Until, &s.Agent} {
		fn(p)
	}
	if s.Exec != nil {
		for i := range s.Exec.Argv {
			fn(&s.Exec.Argv[i])
		}
		fn(&s.Exec.Cwd)
		walkMapStrings(s.Exec.Env, fn)
	}
	if s.Human != nil {
		fn(&s.Human.Assignee)
	}
	if s.Check != nil {
		fn(&s.Check.Contract)
	}
	walkAnyMap(s.With, fn)
	walkAnyMap(s.Outputs, fn)
	fn(&s.ForEach)
}

// walkAnyMap visits every string leaf of m at any depth, in place.
func walkAnyMap(m map[string]any, fn func(*string)) {
	for k, v := range m {
		m[k] = walkAny(v, fn)
	}
}

func walkAny(v any, fn func(*string)) any {
	switch t := v.(type) {
	case string:
		fn(&t)
		return t
	case map[string]any:
		walkAnyMap(t, fn)
		return t
	case []any:
		for i := range t {
			t[i] = walkAny(t[i], fn)
		}
		return t
	}
	return v
}

// cloneAnyMap deep-copies the maps and lists of a decoded document value.
func cloneAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneAny(v)
	}
	return out
}

func cloneAny(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cloneAnyMap(t)
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = cloneAny(t[i])
		}
		return out
	}
	return v
}

func walkMapStrings(m map[string]string, fn func(*string)) {
	for k, v := range m {
		fn(&v)
		m[k] = v
	}
}

// stepRefs lists every placeholder reference in a step's templatable
// fields, in field order, without duplicates.
func stepRefs(s Step) []string {
	var refs []string
	seen := map[string]bool{}
	WalkStepStrings(&s, func(p *string) {
		for _, m := range PlaceholderRe.FindAllStringSubmatch(*p, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				refs = append(refs, m[1])
			}
		}
	})
	return refs
}

// RewriteStepRefs replaces results.<id>. and steps.<id>. references whose
// id is in rename, in placeholders and bare conditions alike, and renames
// a structured due's After the same way.
func RewriteStepRefs(s *Step, rename map[string]string) {
	WalkStepStrings(s, func(p *string) {
		*p = stepRefRe.ReplaceAllStringFunc(*p, func(m string) string {
			sub := stepRefRe.FindStringSubmatch(m)
			if to, ok := rename[sub[2]]; ok {
				return sub[1] + "." + to + "."
			}
			return m
		})
	})
	if to, ok := rename[s.Due.After]; ok {
		s.Due.After = to
	}
}

// substituteVars replaces {{name}} and {{vars.name}} for every bound name
// with its literal replacement text; a binding keyed by a full dotted
// reference (run.iteration) matches that reference exactly. Expansion
// uses it to splice a parent's `with` expressions, a child's defaults and
// the iteration number into block steps.
func substituteVars(s *Step, bindings map[string]string) {
	WalkStepStrings(s, func(p *string) {
		*p = PlaceholderRe.ReplaceAllStringFunc(*p, func(m string) string {
			ref := PlaceholderRe.FindStringSubmatch(m)[1]
			if v, ok := bindings[ref]; ok {
				return v
			}
			name := strings.TrimPrefix(ref, "vars.")
			if strings.Contains(name, ".") {
				return m
			}
			if v, ok := bindings[name]; ok {
				return v
			}
			return m
		})
	})
}
