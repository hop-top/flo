package cel

import (
	"fmt"
	"strings"
)

// Error codes. They are stable; messages are not.
const (
	CodeSyntax        = "syntax_error"
	CodeUndeclaredRef = "undeclared_ref"
	CodeUnknownFunc   = "unknown_function"
	CodeType          = "type_error"
	CodeNotBoolean    = "not_boolean"
	CodeNotList       = "not_list"
	CodeUnterminated  = "unterminated_placeholder"
	CodeUnknownStep   = "unknown_step"
	CodeUnknownState  = "unknown_state_key"
	CodeOutOfScope    = "out_of_scope"
	CodeCycle         = "cycle"
	CodeUnresolved    = "unresolved_ref"
	CodeEval          = "eval_error"
)

// Error is one problem with one expression. Line and Col are 1-based
// positions in the field text the expression was read from (for a bare
// Compile, in the expression itself); zero means unknown. Step and Field
// are filled by the validator.
type Error struct {
	Code  string
	Msg   string
	Step  string
	Field string
	Line  int
	Col   int
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Step != "" {
		fmt.Fprintf(&b, "step %s: ", e.Step)
	}
	if e.Field != "" {
		fmt.Fprintf(&b, "%s: ", e.Field)
	}
	if e.Line > 0 {
		fmt.Fprintf(&b, "%d:%d: ", e.Line, e.Col)
	}
	fmt.Fprintf(&b, "%s (%s)", e.Msg, e.Code)
	return b.String()
}

// Errors is every problem found in one pass. It implements error and
// unwraps to its members.
type Errors []*Error

func (es Errors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "\n")
}

// Unwrap exposes the members to errors.Is and errors.As.
func (es Errors) Unwrap() []error {
	out := make([]error, len(es))
	for i, e := range es {
		out[i] = e
	}
	return out
}

func (es Errors) orNil() error {
	if len(es) == 0 {
		return nil
	}
	return es
}
