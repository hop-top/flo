package cel

import (
	"encoding/json"
	"fmt"
	"strings"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
)

// Eval evaluates the expression against act and returns a plain Go value:
// bool, int64, uint64, float64, string, []byte, nil, []any, map[string]any
// (time.Duration and time.Time for those CEL types). A reference to a
// missing key is an *Error with code unresolved_ref; any other failure has
// code eval_error.
func (x *Expr) Eval(act Activation) (any, error) {
	v, _, err := x.prg.Eval(aliasIn(act))
	if err != nil {
		code := CodeEval
		if strings.Contains(err.Error(), "no such key") || strings.Contains(err.Error(), "no such attribute") {
			code = CodeUnresolved
		}
		return nil, &Error{Code: code, Msg: strings.ReplaceAll(err.Error(), inAlias, "in")}
	}
	return toNative(v), nil
}

func toNative(v ref.Val) any {
	switch t := v.(type) {
	case types.Null:
		return nil
	case traits.Lister:
		out := []any{}
		for it := t.Iterator(); it.HasNext() == types.True; {
			out = append(out, toNative(it.Next()))
		}
		return out
	case traits.Mapper:
		out := map[string]any{}
		for it := t.Iterator(); it.HasNext() == types.True; {
			k := it.Next()
			key, ok := toNative(k).(string)
			if !ok {
				key = fmt.Sprint(toNative(k))
			}
			out[key] = toNative(t.Get(k))
		}
		return out
	}
	return v.Value()
}

// renderText renders a value inside a string with text around the
// placeholder (flow spec §4.3): a string as itself, a list of scalars
// joined with ",", anything else as JSON.
func renderText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			if !isScalar(e) {
				return jsonText(v)
			}
			parts[i] = scalarText(e)
		}
		return strings.Join(parts, ",")
	}
	if isScalar(v) && v != nil {
		return scalarText(v)
	}
	return jsonText(v)
}

func isScalar(v any) bool {
	switch v.(type) {
	case nil, bool, string, int64, uint64, float64:
		return true
	}
	return false
}

func scalarText(v any) string {
	if v == nil {
		return "null"
	}
	if s, ok := v.(string); ok {
		return s
	}
	return jsonText(v)
}

func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// aliasIn returns act with act["in"] also bound under inAlias, where the
// compiled expressions read it. act itself is not modified.
func aliasIn(act Activation) map[string]any {
	in, ok := act["in"]
	if !ok {
		return act
	}
	out := make(map[string]any, len(act)+1)
	for k, val := range act {
		out[k] = val
	}
	out[inAlias] = in
	return out
}
