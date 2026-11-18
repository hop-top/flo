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

// The CEL environment of the spec module does not declare `out`, the parsed
// value that `map` and `fail_when` read (node.md §3.2). Node templates are
// rewritten before compilation: the identifier `out` becomes `item`, a
// declared dynamic slot a node template never uses for anything else.
// String literals and field selections (`a.out`) are left alone. (`in` is
// not rewritten: it relies on the spec module aliasing the CEL keyword.)
const outSlot = "item"

type compiled struct {
	tmpl *cel.Template
	expr *cel.Expr
}

var exprCache sync.Map // string -> *compiled

func (e *Env) compile(src string, rewrite bool) (*compiled, error) {
	key := src
	if rewrite {
		key = "node\x00" + src
	}
	if c, ok := exprCache.Load(key); ok {
		return c.(*compiled), nil //nolint:errcheck,forcetypeassert // only *compiled is stored
	}
	text := src
	if rewrite {
		text = rewriteNode(src)
	}
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
	exprCache.Store(key, &c)
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
		c, cerr := e.compile(def.Map, true)
		if cerr != nil {
			return nil, false, core.Wrap(core.CodeEvalError, cerr)
		}
		act[outSlot] = parsed
		v, verr := c.eval(act)
		if verr != nil {
			return nil, false, evalErr("outputs.map", verr)
		}
		value = v
	}
	if def.FailWhen != "" {
		c, cerr := e.compile(def.FailWhen, true)
		if cerr != nil {
			return nil, false, core.Wrap(core.CodeEvalError, cerr)
		}
		act[outSlot] = value
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
	c, err := e.compile(src, false)
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

// rewriteNode renames `out` to its declared slot in the
// expressions of a template (or in a bare expression), leaving string
// literals, field selections (x.out), the `in` operator and literal text
// outside placeholders alone.
func rewriteNode(src string) string {
	if !strings.Contains(src, "{{") {
		return rewriteExpr(src)
	}
	var b strings.Builder
	i := 0
	for i < len(src) {
		open := strings.Index(src[i:], "{{")
		if open < 0 {
			b.WriteString(src[i:])
			break
		}
		open += i
		b.WriteString(src[i : open+2])
		end := closeAt(src, open+2)
		if end < 0 {
			b.WriteString(src[open+2:])
			break
		}
		b.WriteString(rewriteExpr(src[open+2 : end]))
		b.WriteString("}}")
		i = end + 2
	}
	return b.String()
}

// closeAt returns the index of the `}}` that closes a placeholder opened
// before from, skipping string literals and nested braces; -1 when absent.
func closeAt(s string, from int) int {
	depth := 0
	for i := from; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'', '"':
			i = skipString(s, i)
		case '{':
			depth++
		case '}':
			if depth == 0 && i+1 < len(s) && s[i+1] == '}' {
				return i
			}
			if depth > 0 {
				depth--
			}
		}
	}
	return -1
}

// skipString returns the index of the closing quote of the literal that
// starts at i (len(s)-1 when unterminated).
func skipString(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case q:
			return j
		}
	}
	return len(s) - 1
}

func rewriteExpr(s string) string {
	var b strings.Builder
	prev := byte(0) // last significant byte before the current token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\'' || c == '"':
			j := skipString(s, i)
			b.WriteString(s[i : j+1])
			prev = c
			i = j + 1
		case isIdentStart(c):
			j := i
			for j < len(s) && isIdentPart(s[j]) {
				j++
			}
			word := s[i:j]
			if word == "out" && prev != '.' {
				word = outSlot
			}
			b.WriteString(word)
			prev = 'a'
			i = j
		default:
			b.WriteByte(c)
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				prev = c
			}
			i++
		}
	}
	return b.String()
}

func isIdentStart(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isIdentPart(c byte) bool  { return isIdentStart(c) || (c >= '0' && c <= '9') }
