package node

import (
	"fmt"
	"sort"
	"strings"

	celgo "cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/ext"
)

// Ref is one name a template expression reads: the namespace it starts at
// (in, config, run, out, ...) and the dotted fields selected from it. A
// comprehension variable (`i` in `xs.map(i, i.a)`) is not a Ref.
type Ref struct {
	Root string
	Path []string
}

// String renders root.path.
func (r Ref) String() string {
	if len(r.Path) == 0 {
		return r.Root
	}
	return r.Root + "." + strings.Join(r.Path, ".")
}

// TemplateRefs lists the references of every `{{ }}` placeholder in s, in
// order of appearance. The expressions are parsed, not type-checked, so no
// namespace needs declaring. An unterminated placeholder or an expression
// that does not parse is an error.
func TemplateRefs(s string) ([]Ref, error) {
	var out []Ref
	pos := 0
	for {
		i := strings.Index(s[pos:], "{{")
		if i < 0 {
			return out, nil
		}
		open := pos + i
		end, ok := closePlaceholder(s, open+2)
		if !ok {
			return nil, fmt.Errorf("placeholder {{ at offset %d is never closed by }}", open)
		}
		refs, err := exprRefs(s[open+2 : end])
		if err != nil {
			return nil, err
		}
		out = append(out, refs...)
		pos = end + 2
	}
}

var parseEnv = mustParseEnv()

func mustParseEnv() *celgo.Env {
	env, err := celgo.NewEnv(ext.Lists(), ext.Strings())
	if err != nil {
		panic("node: cel parse env: " + err.Error())
	}
	return env
}

// inAlias stands for the `in` namespace while an expression is parsed: `in`
// is a reserved word of CEL (the membership operator), so `in.repo` does
// not parse as written.
const inAlias = "in__"

// aliasIn rewrites `in.` (an `in` that starts a selection) to inAlias,
// leaving string literals, `x in y` and `.in` alone.
func aliasIn(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c == '\'' || c == '"' {
			j := skipString(s, i)
			b.WriteString(s[i:j])
			i = j
			continue
		}
		if isWordByte(c) {
			j := i
			for j < len(s) && isWordByte(s[j]) {
				j++
			}
			word := s[i:j]
			prevDot := i > 0 && s[i-1] == '.'
			k := j
			for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
				k++
			}
			if word == "in" && !prevDot && k < len(s) && s[k] == '.' {
				word = inAlias
			}
			b.WriteString(word)
			i = j
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func exprRefs(expr string) ([]Ref, error) {
	ast, iss := parseEnv.Parse(aliasIn(expr))
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("expression %q: %w", strings.TrimSpace(expr), iss.Err())
	}
	var out []Ref
	collectRefs(ast.NativeRep().Expr(), map[string]bool{}, &out)
	for i := range out {
		if out[i].Root == inAlias {
			out[i].Root = "in"
		}
	}
	return out, nil
}

func collectRefs(e celast.Expr, bound map[string]bool, out *[]Ref) {
	switch e.Kind() {
	case celast.IdentKind:
		if name := e.AsIdent(); !bound[name] {
			*out = append(*out, Ref{Root: name})
		}
	case celast.SelectKind:
		if root, path, ok := selectChain(e); ok {
			if !bound[root] {
				*out = append(*out, Ref{Root: root, Path: path})
			}
			return
		}
		collectRefs(e.AsSelect().Operand(), bound, out)
	case celast.CallKind:
		call := e.AsCall()
		if call.IsMemberFunction() {
			collectRefs(call.Target(), bound, out)
		}
		for _, a := range call.Args() {
			collectRefs(a, bound, out)
		}
	case celast.ListKind:
		for _, el := range e.AsList().Elements() {
			collectRefs(el, bound, out)
		}
	case celast.MapKind:
		for _, en := range e.AsMap().Entries() {
			me := en.AsMapEntry()
			collectRefs(me.Key(), bound, out)
			collectRefs(me.Value(), bound, out)
		}
	case celast.StructKind:
		for _, f := range e.AsStruct().Fields() {
			collectRefs(f.AsStructField().Value(), bound, out)
		}
	case celast.ComprehensionKind:
		c := e.AsComprehension()
		collectRefs(c.IterRange(), bound, out)
		collectRefs(c.AccuInit(), bound, out)
		inner := make(map[string]bool, len(bound)+3)
		for k := range bound {
			inner[k] = true
		}
		for _, v := range []string{c.IterVar(), c.IterVar2(), c.AccuVar()} {
			if v != "" {
				inner[v] = true
			}
		}
		collectRefs(c.LoopCondition(), inner, out)
		collectRefs(c.LoopStep(), inner, out)
		collectRefs(c.Result(), inner, out)
	case celast.LiteralKind, celast.UnspecifiedExprKind:
	}
}

// selectChain flattens ident.a.b.c into its root and fields; ok is false
// when the chain does not start at an identifier.
func selectChain(e celast.Expr) (root string, path []string, ok bool) {
	var rev []string
	for e.Kind() == celast.SelectKind {
		sel := e.AsSelect()
		rev = append(rev, sel.FieldName())
		e = sel.Operand()
	}
	if e.Kind() != celast.IdentKind {
		return "", nil, false
	}
	for i := len(rev) - 1; i >= 0; i-- {
		path = append(path, rev[i])
	}
	return e.AsIdent(), path, true
}

// roots lists the distinct namespaces of refs, sorted.
func roots(refs []Ref) []string {
	seen := map[string]bool{}
	for _, r := range refs {
		seen[r.Root] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// closePlaceholder returns the index of the `}}` closing a placeholder
// whose expression starts at from. It is CEL-aware (flow spec §4.1):
// string literals and comments are skipped and `}}` closes only at brace
// depth zero.
func closePlaceholder(s string, from int) (int, bool) {
	depth := 0
	for i := from; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'' || c == '"':
			i = skipString(s, i) - 1
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			if nl := strings.IndexByte(s[i:], '\n'); nl >= 0 {
				i += nl - 1
			} else {
				i = len(s)
			}
		case c == '{':
			depth++
		case c == '}':
			if depth > 0 {
				depth--
			} else if i+1 < len(s) && s[i+1] == '}' {
				return i, true
			}
		}
	}
	return 0, false
}

func skipString(s string, i int) int {
	q := s[i]
	raw := (i >= 1 && (s[i-1] == 'r' || s[i-1] == 'R')) ||
		(i >= 2 && (s[i-1] == 'b' || s[i-1] == 'B') && (s[i-2] == 'r' || s[i-2] == 'R'))
	qqq := string([]byte{q, q, q})
	triple := strings.HasPrefix(s[i:], qqq)
	j := i + 1
	if triple {
		j = i + 3
	}
	for j < len(s) {
		switch {
		case !raw && s[j] == '\\':
			j += 2
		case triple && strings.HasPrefix(s[j:], qqq):
			return j + 3
		case !triple && s[j] == q:
			return j + 1
		default:
			j++
		}
	}
	return len(s)
}
