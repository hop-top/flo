package cel

import (
	"sort"
	"strings"

	celgo "cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
)

// Expr is one compiled CEL expression.
type Expr struct {
	src  string
	prg  celgo.Program
	out  *celgo.Type
	refs Refs
}

// Refs are the declared names an expression reads, found in the checked
// syntax tree (comprehension variables are not included). Results and State
// list the keys read as results.<id> / results["<id>"] and state.<key>;
// Namespaces lists every declared top-level name used.
type Refs struct {
	Results    []string
	State      []string
	Namespaces []string
}

// Source is the expression text as written.
func (x *Expr) Source() string { return x.src }

// Refs reports what the expression reads.
func (x *Expr) Refs() Refs { return x.refs }

// OutputType names the static result type ("bool", "list(dyn)", "dyn", ...).
func (x *Expr) OutputType() string { return x.out.String() }

func (x *Expr) outputKind() types.Kind { return x.out.Kind() }

// Compile parses and type-checks one expression. Errors are Errors with
// positions in src. The expression is plain CEL (no {{ }}); ids with "/"
// after results. are rewritten as described in the package doc.
func (e *Env) Compile(src string) (*Expr, error) {
	x, errs := e.compile(src, src, 0)
	if len(errs) > 0 {
		return nil, errs
	}
	return x, nil
}

// compile compiles expr, which starts at byte offset off of field; error
// positions are reported in field.
func (e *Env) compile(expr, field string, off int) (*Expr, Errors) {
	norm, edits := normalizeRefs(expr, e.known)
	at := func(line, col int) (int, int) {
		o := off + mapBack(edits, offsetOf(norm, line, col))
		return lineCol(field, o)
	}
	parsed, iss := e.env.Parse(norm)
	if iss != nil && iss.Err() != nil {
		return nil, issueErrors(iss, at, CodeSyntax, nil)
	}
	calls := callNames(parsed)
	checked, iss := e.env.Check(parsed)
	if iss != nil && iss.Err() != nil {
		return nil, issueErrors(iss, at, CodeType, calls)
	}
	prg, err := e.env.Program(checked)
	if err != nil {
		line, col := lineCol(field, off)
		return nil, Errors{{Code: CodeType, Msg: err.Error(), Line: line, Col: col}}
	}
	return &Expr{src: expr, prg: prg, out: checked.OutputType(), refs: collectRefs(checked)}, nil
}

func issueErrors(iss *celgo.Issues, at func(int, int) (int, int), code string, calls map[string]bool) Errors {
	var out Errors
	for _, ce := range iss.Errors() {
		line, col := at(ce.Location.Line(), ce.Location.Column())
		msg, c := ce.Message, code
		if code == CodeType {
			msg, c = classify(msg, calls)
		}
		out = append(out, &Error{Code: c, Msg: msg, Line: line, Col: col})
	}
	return out
}

// classify maps a checker message to a stable code and drops the
// container suffix the checker appends.
func classify(msg string, calls map[string]bool) (string, string) {
	if rest, ok := strings.CutPrefix(msg, "undeclared reference to '"); ok {
		name, _, _ := strings.Cut(rest, "'")
		if calls[name] {
			return "unknown function '" + name + "'", CodeUnknownFunc
		}
		return "undeclared reference to '" + name + "'", CodeUndeclaredRef
	}
	return msg, CodeType
}

// callNames lists the function names an unchecked expression calls.
func callNames(ast *celgo.Ast) map[string]bool {
	out := map[string]bool{}
	celast.PreOrderVisit(ast.NativeRep().Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		if e.Kind() == celast.CallKind {
			out[e.AsCall().FunctionName()] = true
		}
	}))
	return out
}

func collectRefs(ast *celgo.Ast) Refs {
	native := ast.NativeRep()
	declared := native.ReferenceMap()
	isNS := func(e celast.Expr, name string) bool {
		return e.Kind() == celast.IdentKind && e.AsIdent() == name && declared[e.ID()] != nil
	}
	results, state, spaces := map[string]bool{}, map[string]bool{}, map[string]bool{}
	celast.PreOrderVisit(native.Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		switch e.Kind() {
		case celast.IdentKind:
			if declared[e.ID()] != nil {
				spaces[e.AsIdent()] = true
			}
		case celast.SelectKind:
			sel := e.AsSelect()
			switch {
			case isNS(sel.Operand(), "results"):
				results[sel.FieldName()] = true
			case isNS(sel.Operand(), "state"):
				state[sel.FieldName()] = true
			}
		case celast.CallKind:
			call := e.AsCall()
			args := call.Args()
			if call.FunctionName() != operators.Index || len(args) != 2 || args[1].Kind() != celast.LiteralKind {
				return
			}
			key, ok := args[1].AsLiteral().Value().(string)
			switch {
			case ok && isNS(args[0], "results"):
				results[key] = true
			case ok && isNS(args[0], "state"):
				state[key] = true
			}
		}
	}))
	return Refs{Results: keys(results), State: keys(state), Namespaces: keys(spaces)}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
