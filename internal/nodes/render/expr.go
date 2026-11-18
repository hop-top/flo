package render

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"hop.top/flo/internal/core"
	"hop.top/flo/spec/cel"
	"hop.top/flo/spec/node"
)

type compiled struct {
	tmpl *cel.Template
	expr *cel.Expr
}

var exprCache sync.Map // string -> *compiled

func (e *Env) compile(src string) (*compiled, error) {
	if c, ok := exprCache.Load(src); ok {
		return c.(*compiled), nil //nolint:errcheck,forcetypeassert // only *compiled is stored
	}
	text := src
	var c compiled
	if strings.Contains(text, "{{") {
		t, err := e.env.CompileTemplate(text)
		if err != nil {
			return nil, fmt.Errorf("compile %q: %w", src, err)
		}
		c.tmpl = t
	} else {
		x, err := e.env.Compile(text)
		if err != nil {
			return nil, fmt.Errorf("compile %q: %w", src, err)
		}
		c.expr = x
	}
	exprCache.Store(src, &c)
	return &c, nil
}

func (c *compiled) eval(act cel.Activation) (any, error) {
	if c.tmpl != nil {
		return c.tmpl.Render(act) //nolint:wrapcheck // classified by the caller
	}
	return c.expr.Eval(act) //nolint:wrapcheck // classified by the caller
}

// evalErr classifies an evaluation failure.
func evalErr(what string, err error) *core.Error {
	var ce *cel.Error
	if errors.As(err, &ce) && ce.Code == cel.CodeUnresolved {
		return core.Errorf(core.CodeUnresolvedRef, "%s: %v", what, err)
	}
	return core.Errorf(core.CodeEvalError, "%s: %v", what, err)
}

// Reshape applies `map` then `fail_when` of an output definition (node.md
// §3.2) to the parsed value. It returns the value to validate and whether
// `fail_when` held (the step fails with node_reported_failure, the value
// kept).
func (e *Env) Reshape(def node.OutputDef, parsed any, sc Scope) (value any, failed bool, err *core.Error) {
	value = parsed
	act := sc.Activation()
	if def.Map != "" {
		c, cerr := e.compile(def.Map)
		if cerr != nil {
			return nil, false, core.Wrap(core.CodeEvalError, cerr)
		}
		act["out"] = parsed
		v, verr := c.eval(act)
		if verr != nil {
			return nil, false, evalErr("outputs.map", verr)
		}
		value = v
	}
	if def.FailWhen != "" {
		c, cerr := e.compile(def.FailWhen)
		if cerr != nil {
			return nil, false, core.Wrap(core.CodeEvalError, cerr)
		}
		act["out"] = value
		v, verr := c.eval(act)
		if verr != nil {
			return nil, false, evalErr("outputs.fail_when", verr)
		}
		b, ok := v.(bool)
		if !ok {
			return nil, false, core.Errorf(core.CodeEvalError, "outputs.fail_when: want a boolean, got %T", v)
		}
		failed = b
	}
	return value, failed, nil
}

// ItemFunc evaluates a per-item expression of a built-in node (node.md
// §6.4): `item` and `index` are bound.
type ItemFunc func(item any, index int) (any, error)

// Item compiles a per-item expression: bare CEL, or one {{ }} placeholder.
func (e *Env) Item(src string) (ItemFunc, *core.Error) {
	c, err := e.compile(src)
	if err != nil {
		return nil, core.Errorf(core.CodeInputInvalid, "%v", err)
	}
	return func(item any, index int) (any, error) {
		v, verr := c.eval(cel.Activation{"item": item, "index": int64(index)})
		if verr != nil {
			return nil, evalErr(src, verr)
		}
		return v, nil
	}, nil
}
