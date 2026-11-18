package cel

import (
	"strings"

	celgo "cel.dev/cel-go/cel"

	"hop.top/flo/spec"
)

// Condition is a compiled `when` or `until` (flow spec §4.4).
type Condition struct {
	src    string
	legacy bool
	expr   *Expr
}

// CompileCondition compiles a condition. An expression that is a recipe
// 0.1 single comparison keeps the legacy path (spec.EvalConditionEnv,
// loose equality) and is not compiled as CEL; any other expression is
// CEL and must be boolean. The text may be bare or one {{ }} placeholder.
// An empty condition is always true.
func (e *Env) CompileCondition(src string) (*Condition, error) {
	c, errs := e.compileCondition(src)
	if len(errs) > 0 {
		return nil, errs
	}
	return c, nil
}

func (e *Env) compileCondition(src string) (*Condition, Errors) {
	if strings.TrimSpace(src) == "" || spec.IsLegacyCondition(src) {
		return &Condition{src: src, legacy: strings.TrimSpace(src) != ""}, nil
	}
	expr, off, err := conditionExpr(src)
	if err != nil {
		return nil, Errors{err}
	}
	x, errs := e.compile(expr, src, off)
	if len(errs) > 0 {
		return nil, errs
	}
	if k := x.outputKind(); k != celgo.BoolType.Kind() && k != celgo.DynType.Kind() {
		line, col := lineCol(src, off)
		return nil, Errors{{Code: CodeNotBoolean, Msg: "condition is " + x.OutputType() + ", want bool", Line: line, Col: col}}
	}
	return &Condition{src: src, expr: x}, nil
}

// conditionExpr returns the CEL text of a condition and its offset in src:
// the content of a single placeholder, or the whole text.
func conditionExpr(src string) (string, int, *Error) {
	if !strings.HasPrefix(strings.TrimSpace(src), "{{") {
		return src, 0, nil
	}
	parts, err := scanTemplate(src)
	if err != nil {
		return "", 0, err
	}
	if p, ok := singlePart(parts); ok {
		return p.expr, p.off, nil
	}
	return "", 0, &Error{Code: CodeSyntax, Msg: "a condition is one expression, not several placeholders", Line: 1, Col: 1}
}

// singlePart returns the placeholder when parts is exactly one
// placeholder with only whitespace around it.
func singlePart(parts []tpart) (tpart, bool) {
	var found tpart
	n := 0
	for _, p := range parts {
		switch {
		case p.isExpr:
			found = p
			n++
		case strings.TrimSpace(p.lit) != "":
			return tpart{}, false
		}
	}
	return found, n == 1
}

// Legacy reports whether the condition is a recipe 0.1 single comparison.
func (c *Condition) Legacy() bool { return c.legacy }

// Expr is the compiled CEL expression, nil for an empty or legacy condition.
func (c *Condition) Expr() *Expr { return c.expr }

// Eval evaluates the condition. A legacy comparison reads act["results"]
// (step id to object), act["vars"] and act["in"] as spec.CondEnv does; a
// CEL condition must evaluate to a bool.
func (c *Condition) Eval(act Activation) (bool, error) {
	switch {
	case c.expr != nil:
		v, err := c.expr.Eval(act)
		if err != nil {
			return false, err
		}
		b, ok := v.(bool)
		if !ok {
			return false, &Error{Code: CodeNotBoolean, Msg: "condition evaluated to a non-boolean value"}
		}
		return b, nil
	case c.legacy:
		return spec.EvalConditionEnv(c.src, condEnv(act))
	}
	return true, nil
}

func condEnv(act Activation) spec.CondEnv {
	env := spec.CondEnv{Results: map[string]map[string]any{}}
	env.Vars, _ = act["vars"].(map[string]any)
	env.Inputs, _ = act["in"].(map[string]any)
	if results, ok := act["results"].(map[string]any); ok {
		for id, v := range results {
			if m, isMap := v.(map[string]any); isMap {
				env.Results[id] = m
			}
		}
	}
	return env
}
