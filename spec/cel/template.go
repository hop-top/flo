package cel

import (
	"strings"
)

// Template is a string with {{ }} placeholders, compiled (flow spec §4.1).
type Template struct {
	src   string
	parts []tpart
	exprs []*Expr // parallel to parts; nil for literals
}

// CompileTemplate splits s into literals and placeholders and compiles each
// expression. Errors carry line:col in s; one Errors holds every problem.
// A string with no placeholder is a valid template that renders as itself.
func (e *Env) CompileTemplate(s string) (*Template, error) {
	t, errs := e.compileTemplate(s)
	if len(errs) > 0 {
		return nil, errs
	}
	return t, nil
}

func (e *Env) compileTemplate(s string) (*Template, Errors) {
	parts, serr := scanTemplate(s)
	if serr != nil {
		return nil, Errors{serr}
	}
	t := &Template{src: s, parts: parts, exprs: make([]*Expr, len(parts))}
	var errs Errors
	for i, p := range parts {
		if !p.isExpr {
			continue
		}
		x, xerrs := e.compile(p.expr, s, p.off)
		errs = append(errs, xerrs...)
		t.exprs[i] = x
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return t, nil
}

// Exprs lists the compiled placeholder expressions in order.
func (t *Template) Exprs() []*Expr {
	var out []*Expr
	for _, x := range t.exprs {
		if x != nil {
			out = append(out, x)
		}
	}
	return out
}

// Single returns the expression when the string is exactly one
// placeholder (whitespace around it ignored), else nil.
func (t *Template) Single() *Expr {
	p, ok := singlePart(t.parts)
	if !ok {
		return nil
	}
	for i := range t.parts {
		if t.parts[i].isExpr && t.parts[i].off == p.off {
			return t.exprs[i]
		}
	}
	return nil
}

// Render evaluates the template. A string that is exactly one placeholder
// returns its typed value (a list stays a list, a number a number, null
// nil). Otherwise the result is a string: literals as written, a string
// value as itself, a list of scalars joined with ",", any other value as
// JSON (flow spec §4.3). An unresolvable reference is an *Error, never a
// literal {{ }}.
func (t *Template) Render(act Activation) (any, error) {
	if x := t.Single(); x != nil {
		return x.Eval(act)
	}
	var b strings.Builder
	for i, p := range t.parts {
		if !p.isExpr {
			b.WriteString(p.lit)
			continue
		}
		v, err := t.exprs[i].Eval(act)
		if err != nil {
			return nil, err
		}
		b.WriteString(renderText(v))
	}
	return b.String(), nil
}
