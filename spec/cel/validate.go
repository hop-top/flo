package cel

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	celgo "cel.dev/cel-go/cel"

	"hop.top/flo/spec"
)

// ParseFlow is spec.ParseFlow followed by Validate: the plan-level rules
// first, then every expression compiled. The returned error is spec's, or
// Errors from the expressions.
func ParseFlow(r io.Reader, filename string) (*spec.Flow, error) {
	f, err := spec.ParseFlow(r, filename)
	if err != nil {
		return nil, err
	}
	if err := Validate(f); err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}
	return f, nil
}

// Validate compiles every condition and placeholder of an already
// spec-valid flow and reports every problem as Errors (nil when clean). It
// enforces, for expressions that are CEL (legacy single comparisons are
// spec's):
//
//   - V15: expressions parse and type-check against the declared
//     namespaces and flow vars; functions exist (unknown_function);
//     conditions are boolean; for_each is a list;
//   - V06: every state.<key> names a declared state key;
//   - V13: every results.<id> names a step of the same list (or an
//     enclosing list, or a block member deferred to expansion), never the
//     step itself, and the graph with those edges added stays acyclic;
//   - V14: result only in outputs and check; item and index only under
//     for_each or a uses step (a node may bind them), never in for_each.
//
// Trigger `when` and `source` are checked against trigger only.
func Validate(f *spec.Flow) error {
	env, err := EnvForFlow(f)
	if err != nil {
		return err
	}
	v := &validator{f: f, env: env}
	v.list(f.Steps, nil)
	v.returns()
	v.triggers()
	return v.errs.orNil()
}

type validator struct {
	f    *spec.Flow
	env  *Env
	errs Errors
}

func (v *validator) report(step, field string, errs ...*Error) {
	for _, e := range errs {
		if e.Step == "" {
			e.Step = step
		}
		if e.Field == "" {
			e.Field = field
		}
		v.errs = append(v.errs, e)
	}
}

// scope is where a step's results references resolve.
type scope struct {
	ids     map[string]bool // steps of the list
	outer   map[string]bool // steps of enclosing lists
	members map[string]bool // for a block's until: its nested steps
}

func (v *validator) list(steps []spec.Step, outer map[string]bool) {
	ids := make(map[string]bool, len(steps))
	for i := range steps {
		ids[steps[i].ID] = true
	}
	inner := make(map[string]bool, len(outer)+len(ids))
	for id := range outer {
		inner[id] = true
	}
	for id := range ids {
		inner[id] = true
	}
	edges := map[string][]string{}
	for i := range steps {
		s := &steps[i]
		sc := scope{ids: ids, outer: outer, members: map[string]bool{}}
		for j := range s.Steps {
			sc.members[s.Steps[j].ID] = true
		}
		edges[s.ID] = v.step(s, sc)
		if s.IsRepeat() {
			v.list(s.Steps, inner)
		}
	}
	v.cycles(steps, edges)
}

// step compiles every templatable field of s and returns the in-list
// steps its CEL expressions read.
func (v *validator) step(s *spec.Step, sc scope) []string {
	var deps []string
	for _, field := range spec.StepFields(s) {
		var xs []*Expr
		switch field.Path {
		case "when", "until":
			c, errs := v.env.compileCondition(field.Value)
			v.report(s.ID, field.Path, errs...)
			if c != nil && c.expr != nil {
				xs = append(xs, c.expr)
			}
		default:
			t, errs := v.env.compileTemplate(field.Value)
			v.report(s.ID, field.Path, errs...)
			if t == nil {
				continue
			}
			xs = t.Exprs()
			if field.Path == "for_each" {
				v.forEach(s.ID, t)
			}
		}
		for _, x := range xs {
			deps = append(deps, v.refs(s, field.Path, x, sc)...)
		}
	}
	return deps
}

func (v *validator) forEach(stepID string, t *Template) {
	x := t.Single()
	if x == nil {
		v.report(stepID, "for_each", &Error{Code: CodeNotList, Msg: "for_each is exactly one placeholder evaluating to a list", Line: 1, Col: 1})
		return
	}
	if k := x.outputKind(); k != celgo.ListType(celgo.DynType).Kind() && k != celgo.DynType.Kind() {
		v.report(stepID, "for_each", &Error{Code: CodeNotList, Msg: "for_each is " + x.OutputType() + ", want a list", Line: 1, Col: 1})
	}
}

// refs checks the results, state and namespace references of x and returns
// the in-list step ids it reads.
func (v *validator) refs(s *spec.Step, field string, x *Expr, sc scope) []string {
	var deps []string
	r := x.Refs()
	for _, target := range r.Results {
		switch {
		case target == s.ID && field != "until":
			v.report(s.ID, field, &Error{Code: CodeCycle, Msg: "references its own results"})
		case sc.ids[target]:
			deps = append(deps, target)
		case sc.outer[target], strings.Contains(target, "/"), field == "until" && sc.members[target]:
			// enclosing list or block member: checked after expansion
		default:
			v.report(s.ID, field, &Error{Code: CodeUnknownStep, Msg: fmt.Sprintf("references unknown step %q", target)})
		}
	}
	for _, key := range r.State {
		if _, ok := v.f.State[key]; !ok {
			v.report(s.ID, field, &Error{Code: CodeUnknownState, Msg: fmt.Sprintf("references undeclared state key %q; declare it under state", key)})
		}
	}
	for _, ns := range r.Namespaces {
		if msg := scopeViolation(s, field, ns); msg != "" {
			v.report(s.ID, field, &Error{Code: CodeOutOfScope, Msg: msg})
		}
	}
	return deps
}

// scopeViolation reports a namespace used where it is not bound (V14).
func scopeViolation(s *spec.Step, field, ns string) string {
	switch ns {
	case "result":
		if !strings.HasPrefix(field, "outputs") && !strings.HasPrefix(field, "check") {
			return "result is bound only in outputs and check"
		}
	case "item", "index":
		switch {
		case field == "for_each":
			return ns + " is bound per element, not in the for_each expression"
		case s.ForEach == "" && !s.IsUses():
			return ns + " is bound only under for_each or in a node's own expressions"
		}
	}
	return ""
}

// cycles reports a cycle in depends_on, results edges and the CEL results
// edges of one list.
func (v *validator) cycles(steps []spec.Step, celEdges map[string][]string) {
	ids := map[string]bool{}
	for i := range steps {
		ids[steps[i].ID] = true
	}
	edges := map[string][]string{}
	for i := range steps {
		s := &steps[i]
		all := slices.Concat(s.DependsOn, spec.ImplicitDeps(*s), celEdges[s.ID])
		edges[s.ID] = slices.DeleteFunc(all, func(id string) bool { return !ids[id] })
	}
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(id string) bool
	visit = func(id string) bool {
		state[id] = 1
		for _, dep := range edges[id] {
			if state[dep] == 1 {
				v.report(id, "", &Error{Code: CodeCycle, Msg: fmt.Sprintf("dependency cycle through step %q", dep)})
				return true
			}
			if state[dep] == 0 && visit(dep) {
				return true
			}
		}
		state[id] = 2
		return false
	}
	for i := range steps {
		if state[steps[i].ID] == 0 && visit(steps[i].ID) {
			return
		}
	}
}

// returns checks the sub-flow output expressions (flow spec §5.9).
func (v *validator) returns() {
	names := make([]string, 0, len(v.f.Returns))
	for n := range v.f.Returns {
		names = append(names, n)
	}
	sort.Strings(names)
	top := &spec.Step{}
	sc := scope{ids: map[string]bool{}, outer: map[string]bool{}}
	for i := range v.f.Steps {
		sc.ids[v.f.Steps[i].ID] = true
	}
	for _, n := range names {
		field := "returns." + n
		t, errs := v.env.compileTemplate(v.f.Returns[n])
		v.report("", field, errs...)
		if t == nil {
			continue
		}
		for _, x := range t.Exprs() {
			v.refs(top, field, x, sc)
		}
	}
}

// triggers checks `when` and `source` of every on: entry against the
// trigger namespace only (flow spec §4.2).
func (v *validator) triggers() {
	env, err := newTriggerEnv()
	if err != nil {
		return
	}
	for i, entry := range v.f.On {
		for kind, cfg := range entry {
			conf, ok := cfg.(map[string]any)
			if !ok {
				continue
			}
			for _, key := range []string{"when", "source"} {
				text, ok := conf[key].(string)
				if !ok {
					continue
				}
				field := fmt.Sprintf("on[%d].%s.%s", i, kind, key)
				if key == "when" {
					_, errs := env.compileCondition(text)
					v.report("", field, errs...)
					continue
				}
				_, errs := env.compileTemplate(text)
				v.report("", field, errs...)
			}
		}
	}
}

// ImplicitDeps lists the sibling step ids s reads through results, in
// either spelling: spec.ImplicitDeps (legacy placeholders and legacy
// conditions) plus every results reference of its CEL expressions
// (flow spec §5.4). A block's own until is not included. A plan-level
// implementer that wires dependencies must use this, not spec's alone,
// when flows carry CEL.
func (e *Env) ImplicitDeps(s *spec.Step) []string {
	out := spec.ImplicitDeps(*s)
	add := func(id string) {
		if id != s.ID && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	for _, field := range spec.StepFields(s) {
		if field.Path == "until" {
			continue
		}
		if field.Path == "when" {
			if c, errs := e.compileCondition(field.Value); len(errs) == 0 && c.expr != nil {
				for _, id := range c.expr.refs.Results {
					add(id)
				}
			}
			continue
		}
		if t, errs := e.compileTemplate(field.Value); len(errs) == 0 {
			for _, x := range t.Exprs() {
				for _, id := range x.refs.Results {
					add(id)
				}
			}
		}
	}
	return out
}
