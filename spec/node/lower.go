package node

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"hop.top/flo/spec"
)

// DefaultTimeout is a node's per-attempt timeout when neither the node nor
// the step sets one (node.md §1.1).
const DefaultTimeout = "30s"

// Lowering reports what the node becomes when a flow is planned (node.md
// §14). The author's `lowers_to` decides; absent, a command node lowers to
// exec when ExecLowerable says it may, and everything else is engine.
func (s *Spec) Lowering() LowersTo {
	if s.LowersTo != "" {
		return s.LowersTo
	}
	if s.Kind == KindCommand && ExecLowerable(s) == "" {
		return LowersExec
	}
	return LowersEngine
}

// RenderFunc renders one string against scope (the namespaces `in`,
// `config` and `run`). A string that is exactly one placeholder returns
// its typed value, any other string a string (flow.md §4.3). The spec
// module parses templates but does not evaluate CEL; the caller supplies
// the evaluator (spec/cel in the engine).
type RenderFunc func(tmpl string, scope map[string]any) (any, error)

// Lowered is the outcome of Lower.
type Lowered struct {
	// To is exec or engine.
	To LowersTo
	// Step is the plain exec step a plan-level tool can run; set only when
	// To is exec. It keeps the original step's id, dependencies, when,
	// title, retry and so on.
	Step *spec.Step
}

// Lower lowers a `uses:` step whose node is s. For an engine node it only
// reports the target. For an exec node it binds `with` (plus input
// defaults) as `in`, renders argv, env and cwd through render, and returns
// the equivalent exec step: Uses and With cleared, Kind exec, Exec holding
// the rendered argv, the timeout (the step's, else the node's, else 30s)
// and stdout cap, the node's retry as the step's default, and the
// idempotency key in FLO_IDEMPOTENCY_KEY (node.md §11).
//
// config is the pack configuration (config.<ns>.<key>); run is the run
// namespace (a plan-level tool has no step_dir, so a node that reads it
// does not lower). A `with` value that is itself a template over results
// stays a string in `in`; the plan-level renderer resolves it later.
func Lower(s *Spec, st spec.Step, render RenderFunc, config, run map[string]any) (Lowered, error) {
	if s.Lowering() != LowersExec {
		return Lowered{To: LowersEngine}, nil
	}
	if why := ExecLowerable(s); why != "" {
		return Lowered{}, &Error{Code: CodeInvalidNode, Field: "lowers_to", Msg: "cannot lower to exec: " + why}
	}
	in, err := bindInputs(s, st)
	if err != nil {
		return Lowered{}, err
	}
	scope := map[string]any{"in": in, "config": orEmpty(config), "run": orEmpty(run)}
	var argv []string
	for i, a := range s.Argv {
		v, err := render(a, scope)
		if err != nil {
			return Lowered{}, fmt.Errorf("argv[%d]: %w", i, err)
		}
		if list, isList := v.([]any); isList && isWholePlaceholder(a) {
			for _, e := range list {
				argv = append(argv, scalar(e))
			}
			continue
		}
		argv = append(argv, scalar(v))
	}
	ex := &spec.ExecSpec{Argv: argv, Timeout: firstNonEmpty(st.Timeout, s.Timeout, DefaultTimeout), StdoutMax: s.StdoutMax}
	if ex.Cwd, err = renderString(render, s.Cwd, scope, "cwd"); err != nil {
		return Lowered{}, err
	}
	if len(s.Env) > 0 || s.IdempotencyKey != "" {
		ex.Env = map[string]string{}
		for _, k := range sortedStringKeys(s.Env) {
			if ex.Env[k], err = renderString(render, s.Env[k], scope, "env."+k); err != nil {
				return Lowered{}, err
			}
		}
		if s.IdempotencyKey != "" {
			if ex.Env["FLO_IDEMPOTENCY_KEY"], err = renderString(render, s.IdempotencyKey, scope, "idempotency_key"); err != nil {
				return Lowered{}, err
			}
		}
	}
	out := st
	out.Uses, out.With, out.Kind, out.Exec = "", nil, spec.KindExec, ex
	if out.Retry == nil && s.Retry != nil {
		out.Retry = &spec.RetrySpec{MaxAttempts: s.Retry.MaxAttempts, Backoff: s.Retry.Backoff}
	}
	return Lowered{To: LowersExec, Step: &out}, nil
}

// bindInputs builds `in`: the step's with values, then defaults for inputs
// it leaves out. A required input with neither is an error; so is a with
// key the node does not declare.
func bindInputs(s *Spec, st spec.Step) (map[string]any, error) {
	in := map[string]any{}
	for k, v := range st.With {
		if _, ok := s.Inputs[k]; !ok {
			return nil, &Error{Code: CodeInvalidNode, Field: "with." + k, Msg: fmt.Sprintf("%q is not an input of %s", k, s.Node)}
		}
		in[k] = v
	}
	for _, name := range sortedDefs(s.Inputs) {
		def := s.Inputs[name]
		if _, ok := in[name]; ok {
			continue
		}
		switch {
		case def.HasDefault():
			in[name] = def.Default
		case def.Required.Flag:
			return nil, &Error{Code: CodeInvalidNode, Field: "with." + name, Msg: fmt.Sprintf("required input %q of %s is missing", name, s.Node)}
		}
	}
	return in, nil
}

func isWholePlaceholder(s string) bool {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "{{") || !strings.HasSuffix(t, "}}") {
		return false
	}
	end, ok := closePlaceholder(t, 2)
	return ok && end+2 == len(t)
}

func renderString(render RenderFunc, tmpl string, scope map[string]any, field string) (string, error) {
	if tmpl == "" {
		return "", nil
	}
	v, err := render(tmpl, scope)
	if err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	return scalar(v), nil
}

// scalar renders a value as argv, env and cwd text: a string as itself,
// null as empty (node.md §4.1), a list of scalars joined with ",", anything
// else as JSON (flow.md §4.3).
func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		parts := make([]string, len(t))
		allScalar := true
		for i, e := range t {
			switch e.(type) {
			case map[string]any, []any:
				allScalar = false
			}
			parts[i] = scalar(e)
		}
		if allScalar {
			return strings.Join(parts, ",")
		}
	case bool, int, int64, float64:
		return fmt.Sprint(t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func sortedStringKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
