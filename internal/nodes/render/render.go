// Package render evaluates the templates of a node spec (node.md §3.1, §4.1,
// §5.1): the `argv`, `env`, `cwd` and request values of a declared node,
// the `map` and `fail_when` of its outputs, and the per-item expressions of
// the built-in nodes. It wraps the CEL environment of the spec module.
package render

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec"
	"hop.top/flo/spec/cel"
)

// DefaultTimeout is the per-attempt bound of a node that declares none
// (node.md §1.1).
const DefaultTimeout = 30 * time.Second

// Timeout returns the bound of one attempt: the invocation's, else the
// node's own, else DefaultTimeout.
func Timeout(inv core.Invocation) time.Duration {
	if inv.Timeout > 0 {
		return inv.Timeout
	}
	if inv.Node != nil && inv.Node.Timeout != "" {
		if d, err := spec.ParseDuration(inv.Node.Timeout); err == nil && d > 0 {
			return d
		}
	}
	return DefaultTimeout
}

// Fail classifies a render or evaluation failure: a reference with no value
// is unresolved_ref, anything else eval_error.
func Fail(what string, err error) *core.Error { return evalErr(what, err) }

// Env compiles and renders templates. It is safe for concurrent use.
type Env struct {
	env *cel.Env
}

var (
	sharedOnce sync.Once
	shared     *Env
	sharedErr  error
)

// Shared returns the process-wide environment.
func Shared() (*Env, error) {
	sharedOnce.Do(func() {
		e, err := cel.NewEnv()
		if err != nil {
			sharedErr = err
			return
		}
		shared = &Env{env: e}
	})
	return shared, sharedErr
}

// Scope is what a node template reads (node.md §3.1): `in`, `config` and
// `run`, and nothing else.
type Scope struct {
	In     map[string]any
	Config map[string]any
	Run    core.RunContext
	// StepDir is `run.step_dir`, the scratch directory of one attempt.
	StepDir string
}

// Activation returns the CEL activation of the scope.
func (s Scope) Activation() cel.Activation {
	run := map[string]any{
		"id":         string(s.Run.ID),
		"flow":       s.Run.Flow,
		"version":    s.Run.Version,
		"started_at": s.Run.StartedAt.UTC().Format(time.RFC3339),
		"iteration":  int64(s.Run.Iteration),
		"step_dir":   s.StepDir,
	}
	in := s.In
	if in == nil {
		in = map[string]any{}
	}
	cfg := s.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cel.Activation{"in": in, inSlot: in, "config": cfg, "run": run}
}

// Value renders a data tree: string leaves are templates; a leaf that is
// one whole placeholder keeps its type (flow.md §4.3). Map keys are not
// templated.
func (e *Env) Value(v any, act cel.Activation) (any, error) {
	switch t := v.(type) {
	case string:
		if !strings.Contains(t, "{{") {
			return t, nil
		}
		c, err := e.compile(t, true)
		if err != nil {
			return nil, err
		}
		out, err := c.eval(act)
		if err != nil {
			return nil, fmt.Errorf("render: %w", err)
		}
		return out, nil
	case []string:
		items := make([]any, len(t))
		for i, s := range t {
			items[i] = s
		}
		return e.Value(items, act)
	case []any:
		out := make([]any, len(t))
		for i, it := range t {
			r, err := e.Value(it, act)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = r
		}
		return out, nil
	case map[string]string:
		m := make(map[string]any, len(t))
		for k, s := range t {
			m[k] = s
		}
		return e.Value(m, act)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, it := range t {
			r, err := e.Value(it, act)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = r
		}
		return out, nil
	}
	return v, nil
}

// String renders a string template to text. A null renders as "".
func (e *Env) String(s string, act cel.Activation) (string, error) {
	v, err := e.Value(s, act)
	if err != nil {
		return "", err
	}
	return Text(v), nil
}

// Argv renders an argv (node.md §4.1): each element is a template; an
// element that is one placeholder with a list value expands into several
// arguments; a null renders as "". Nothing is passed through a shell.
func (e *Env) Argv(argv []string, act cel.Activation) ([]string, error) {
	out := make([]string, 0, len(argv))
	for i, a := range argv {
		v, err := e.Value(a, act)
		if err != nil {
			return nil, fmt.Errorf("argv[%d]: %w", i, err)
		}
		if list, ok := v.([]any); ok {
			for _, it := range list {
				out = append(out, Text(it))
			}
			continue
		}
		out = append(out, Text(v))
	}
	return out, nil
}

// StringMap renders a map of string templates to text (env values).
func (e *Env) StringMap(m map[string]string, act cel.Activation) (map[string]string, error) {
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, err := e.String(v, act)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = s
	}
	return out, nil
}

// Text renders a value as the text of an argument: nil is "", a string is
// itself, a number or boolean its literal form, a list of scalars is joined
// with ",", anything else is JSON.
func Text(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case []any:
		parts := make([]string, len(t))
		for i, it := range t {
			parts[i] = Text(it)
		}
		return strings.Join(parts, ",")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
