package spec

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// CondEnv is what a condition expression can read.
//
//   - Results: upstream task results keyed by step id, read as
//     results.<step>.<dotted path> — the executor's `when:` input.
//   - Vars: flow vars, read as vars.<name>.
//   - Inputs: named inputs, read as inputs.<name>; a bare identifier
//     resolves in Inputs first, then Vars.
type CondEnv struct {
	Inputs  map[string]any
	Vars    map[string]any
	Results map[string]map[string]any
}

// EvalConditionEnv evaluates a condition against env.
//
// Grammar (intentionally tiny — no expr/CEL dependency):
//
//	expr   := lhs OP rhs
//	lhs    := "results." STEP "." PATH  |  "vars." IDENT  |  "inputs." IDENT  |  IDENT
//	OP     := "=="  |  "!="  |  "<"  |  "<="  |  ">"  |  ">="
//	rhs    := "'" STRING "'"  |  '"' STRING '"'  |  NUMBER  |  true  |  false  |  IDENT
//
// Whitespace around tokens is tolerated. Empty expr returns true (no
// gate). Equality compares the value's %v rendering with the literal, so
// numbers and booleans match their unquoted or quoted spelling. Ordering
// operators require both sides to be numbers. An unknown step, path or
// key is an error so callers fail loudly rather than silently skip.
func EvalConditionEnv(expr string, env CondEnv) (bool, error) {
	if strings.TrimSpace(expr) == "" {
		return true, nil
	}
	c, legacy, err := classifyCondition(expr)
	if err != nil {
		return false, err
	}
	if !legacy {
		return false, fmt.Errorf("condition %q: %w", expr, ErrNeedsCEL)
	}

	val, err := env.lookup(c.lhs)
	if err != nil {
		return false, fmt.Errorf("condition %q: %w", expr, err)
	}
	rhsVal := unquote(c.rhs)

	switch c.op {
	case "==":
		return fmt.Sprintf("%v", val) == rhsVal, nil
	case "!=":
		return fmt.Sprintf("%v", val) != rhsVal, nil
	}

	lhsNum, ok := toFloat(val)
	if !ok {
		return false, fmt.Errorf("condition %q: %s is not a number (%v), cannot compare with %s", expr, c.lhs, val, c.op)
	}
	rhsNum, err := strconv.ParseFloat(rhsVal, 64)
	if err != nil {
		return false, fmt.Errorf("condition %q: %q is not a number, cannot compare with %s", expr, rhsVal, c.op)
	}
	switch c.op {
	case "<":
		return lhsNum < rhsNum, nil
	case "<=":
		return lhsNum <= rhsNum, nil
	case ">":
		return lhsNum > rhsNum, nil
	default: // ">="
		return lhsNum >= rhsNum, nil
	}
}

// ErrNeedsCEL is returned when a condition is a CEL expression rather than
// a single comparison of the recipe 0.1 grammar. The spec module evaluates
// only the legacy form; a CEL environment belongs to the engine.
var ErrNeedsCEL = errors.New("expression needs a CEL engine (not a single comparison)")

var (
	legacyLHS = regexp.MustCompile(`^(results\.\S+|vars\.[A-Za-z_]\w*|inputs\.[A-Za-z_]\w*|[A-Za-z_]\w*)$`)
	legacyRHS = regexp.MustCompile(`^('[^']*'|"[^"]*"|-?[0-9]+(\.[0-9]+)?|true|false|[A-Za-z_]\w*)$`)
)

// unwrapCondition strips one surrounding {{ }} so a condition may be
// written bare or as a single placeholder.
func unwrapCondition(expr string) string {
	expr = strings.TrimSpace(expr)
	if inner, ok := strings.CutPrefix(expr, "{{"); ok {
		if inner, ok = strings.CutSuffix(inner, "}}"); ok && !strings.Contains(inner, "{{") && !strings.Contains(inner, "}}") {
			return strings.TrimSpace(inner)
		}
	}
	return expr
}

// classifyCondition reports whether expr is the legacy single comparison
// (legacy true, parsed in c) or a CEL expression (legacy false; the text
// is not parsed here). It returns an error only for a legacy-shaped
// expression that is malformed in a way the grammar can name: a
// results reference without step or field.
func classifyCondition(expr string) (c condition, legacy bool, err error) {
	c, perr := parseCondition(unwrapCondition(expr))
	if perr != nil || !legacyLHS.MatchString(c.lhs) || !legacyRHS.MatchString(c.rhs) {
		return condition{}, false, nil
	}
	return c, true, nil
}

// IsLegacyCondition reports whether expr is the recipe 0.1 single
// comparison (loose equality, evaluated by EvalConditionEnv) rather than a
// CEL expression. A CEL implementation tries this first so existing
// conditions keep their meaning.
func IsLegacyCondition(expr string) bool {
	_, legacy, err := classifyCondition(expr)
	return err == nil && legacy
}

// negateExpr returns an expression true exactly when expr is false: the
// operator flip for a legacy comparison, !( … ) for CEL.
func negateExpr(expr string) string {
	if c, legacy, err := classifyCondition(expr); err == nil && legacy {
		return c.negate().String()
	}
	return "!(" + unwrapCondition(expr) + ")"
}

// condition is a parsed `lhs OP rhs` expression. rhs keeps its quotes so
// String round-trips the author's spelling.
type condition struct {
	lhs, op, rhs string
}

// parseCondition splits expr into its three tokens without evaluating
// it. Validation uses it to check grammar and to find results.<step>
// references; expansion uses it to derive a negated `until`.
func parseCondition(expr string) (condition, error) {
	expr = strings.TrimSpace(expr)
	op, opIdx := findOperator(expr)
	if opIdx < 0 {
		return condition{}, fmt.Errorf("invalid condition %q: expected one of == != < <= > >=", expr)
	}
	c := condition{
		lhs: strings.TrimSpace(expr[:opIdx]),
		op:  op,
		rhs: strings.TrimSpace(expr[opIdx+len(op):]),
	}
	if c.lhs == "" || c.rhs == "" {
		return condition{}, fmt.Errorf("invalid condition %q: empty operand", expr)
	}
	if step, _, isResult := c.resultsRef(); isResult && step == "" {
		return condition{}, fmt.Errorf("invalid condition %q: want results.<step>[.<field>]", expr)
	}
	return c, nil
}

// String renders the condition with single spaces around the operator.
func (c condition) String() string { return c.lhs + " " + c.op + " " + c.rhs }

// negate flips the operator so the condition is true exactly when the
// original is false.
func (c condition) negate() condition {
	flip := map[string]string{"==": "!=", "!=": "==", "<": ">=", ">=": "<", ">": "<=", "<=": ">"}
	return condition{lhs: c.lhs, op: flip[c.op], rhs: c.rhs}
}

// resultsRef splits a results.<step>.<path> lhs into its step and path.
func (c condition) resultsRef() (step, path string, ok bool) {
	rest, isResult := strings.CutPrefix(c.lhs, "results.")
	if !isResult {
		return "", "", false
	}
	step, path, _ = strings.Cut(rest, ".")
	return step, path, true
}

// lookup resolves an lhs reference in the environment.
func (env CondEnv) lookup(ref string) (any, error) {
	switch {
	case strings.HasPrefix(ref, "results."):
		return env.lookupResult(strings.TrimPrefix(ref, "results."))
	case strings.HasPrefix(ref, "vars."):
		return lookupKey(env.Vars, strings.TrimPrefix(ref, "vars."), "var")
	case strings.HasPrefix(ref, "inputs."):
		return lookupKey(env.Inputs, strings.TrimPrefix(ref, "inputs."), "input")
	}
	if v, ok := env.Inputs[ref]; ok {
		return v, nil
	}
	if v, ok := env.Vars[ref]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("references unknown key %q", ref)
}

// lookupResult resolves "<step>.<dotted path>" into Results.
func (env CondEnv) lookupResult(path string) (any, error) {
	step, rest, hasPath := strings.Cut(path, ".")
	if step == "" {
		return nil, fmt.Errorf("results reference %q must be results.<step>[.<field>]", path)
	}
	result, found := env.Results[step]
	if !found {
		return nil, fmt.Errorf("references unknown step %q", step)
	}
	if !hasPath {
		return result, nil // results.<step> is the step's whole output
	}
	var cur any = result
	for _, part := range strings.Split(rest, ".") {
		m, isMap := cur.(map[string]any)
		if !isMap {
			return nil, fmt.Errorf("results.%s.%s: %q is not an object", step, rest, part)
		}
		next, ok := m[part]
		if !ok {
			return nil, fmt.Errorf("references unknown field %q of step %q", part, step)
		}
		cur = next
	}
	return cur, nil
}

func lookupKey(m map[string]any, key, kind string) (any, error) {
	v, ok := m[key]
	if !ok {
		return nil, fmt.Errorf("references unknown %s key %q", kind, key)
	}
	return v, nil
}

// toFloat widens any numeric value (including a numeric string) to float64.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

// conditionOperators in matching order: two-character operators first so
// "<=" is never read as "<" followed by "= …".
var conditionOperators = []string{"==", "!=", "<=", ">=", "<", ">"}

// findOperator returns the earliest operator in expr and its byte index;
// at equal positions the longer operator wins.
func findOperator(expr string) (string, int) {
	bestOp, bestIdx := "", -1
	for _, op := range conditionOperators {
		idx := strings.Index(expr, op)
		if idx < 0 {
			continue
		}
		if bestIdx < 0 || idx < bestIdx {
			bestOp, bestIdx = op, idx
		}
	}
	return bestOp, bestIdx
}

// unquote strips a single matching pair of single or double quotes. Bare
// identifiers and numbers are returned as-is.
func unquote(s string) string {
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '\'' && last == '\'') || (first == '"' && last == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
