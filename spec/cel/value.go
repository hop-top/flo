package cel

import (
	"fmt"
	"sort"
)

// Value is a data tree (the `with` of a step, a request body, an
// `outputs.state` map) whose string leaves are compiled templates. Compile
// once with CompileValue, then Render per element or per attempt.
type Value struct {
	kind   valueKind
	lit    any         // scalar, for vkLiteral
	tmpl   *Template   // for vkTemplate
	list   []*Value    // for vkList
	fields []valueItem // for vkMap, sorted by key
}

type valueKind int

const (
	vkLiteral valueKind = iota
	vkTemplate
	vkList
	vkMap
)

type valueItem struct {
	key string
	val *Value
}

// CompileValue compiles every string leaf of v as a template (flow spec
// §4.3): maps and lists are walked, strings become templates, every other
// scalar is kept as it is. []string, []any, map[string]string and
// map[string]any are walked; map keys are not templated. Errors carry the
// path of the leaf in Field (`with.repos[0]` style, rooted at root).
func (e *Env) CompileValue(root string, v any) (*Value, error) {
	val, errs := e.compileValue(root, v)
	if len(errs) > 0 {
		return nil, errs
	}
	return val, nil
}

func (e *Env) compileValue(path string, v any) (*Value, Errors) {
	switch t := v.(type) {
	case string:
		tmpl, errs := e.compileTemplate(t)
		if len(errs) > 0 {
			return nil, atPath(errs, path)
		}
		if len(tmpl.Exprs()) == 0 {
			return &Value{kind: vkLiteral, lit: t}, nil
		}
		return &Value{kind: vkTemplate, tmpl: tmpl}, nil
	case []any:
		return e.compileList(path, t)
	case []string:
		items := make([]any, len(t))
		for i, s := range t {
			items[i] = s
		}
		return e.compileList(path, items)
	case map[string]any:
		return e.compileMap(path, t)
	case map[string]string:
		m := make(map[string]any, len(t))
		for k, s := range t {
			m[k] = s
		}
		return e.compileMap(path, m)
	}
	return &Value{kind: vkLiteral, lit: v}, nil
}

func (e *Env) compileList(path string, items []any) (*Value, Errors) {
	out := &Value{kind: vkList, list: make([]*Value, len(items))}
	var errs Errors
	for i, it := range items {
		c, cerrs := e.compileValue(fmt.Sprintf("%s[%d]", path, i), it)
		errs = append(errs, cerrs...)
		out.list[i] = c
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}

func (e *Env) compileMap(path string, m map[string]any) (*Value, Errors) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := &Value{kind: vkMap, fields: make([]valueItem, 0, len(keys))}
	var errs Errors
	for _, k := range keys {
		c, cerrs := e.compileValue(joinPath(path, k), m[k])
		errs = append(errs, cerrs...)
		out.fields = append(out.fields, valueItem{key: k, val: c})
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// atPath returns a copy of errs with Field set to path where it is empty.
func atPath(errs Errors, path string) Errors {
	out := make(Errors, len(errs))
	for i, e := range errs {
		c := *e
		if c.Field == "" {
			c.Field = path
		}
		out[i] = &c
	}
	return out
}

// Render evaluates the tree against act and returns a fresh value of plain
// Go types: map[string]any, []any and scalars. A string that is exactly one
// placeholder keeps the type of its value (a list stays a list, a number a
// number, null nil); a string with text around a placeholder renders to a
// string (§4.3). The first unresolvable reference is returned as an *Error
// (code unresolved_ref) with Field set to the leaf's path, so the caller can
// tell an absent result (§5.7) from another failure.
func (v *Value) Render(act Activation) (any, error) {
	return v.render("", act)
}

func (v *Value) render(path string, act Activation) (any, error) {
	switch v.kind {
	case vkLiteral:
		return v.lit, nil
	case vkTemplate:
		out, err := v.tmpl.Render(act)
		if err != nil {
			return nil, withField(err, path)
		}
		return out, nil
	case vkList:
		out := make([]any, len(v.list))
		for i, it := range v.list {
			r, err := it.render(fmt.Sprintf("%s[%d]", path, i), act)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	default:
		out := make(map[string]any, len(v.fields))
		for _, f := range v.fields {
			r, err := f.val.render(joinPath(path, f.key), act)
			if err != nil {
				return nil, err
			}
			out[f.key] = r
		}
		return out, nil
	}
}

func withField(err error, path string) error {
	if ce, ok := err.(*Error); ok && ce.Field == "" { //nolint:errorlint // Render returns *Error directly
		c := *ce
		c.Field = path
		return &c
	}
	return err
}

// RenderValue compiles and renders v in one call: the walk of flow spec
// §4.3 over a `with` map, a body or a list. Use CompileValue and Render when
// the same tree is rendered more than once (per for_each element, per retry).
func (e *Env) RenderValue(v any, act Activation) (any, error) {
	c, err := e.CompileValue("", v)
	if err != nil {
		return nil, err
	}
	return c.Render(act)
}
