package run

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"hop.top/flo/spec"
)

// CodeInvalidVar is the code of every variable error: a value that cannot
// be coerced to the var's declared type, or a var the flow does not take.
const CodeInvalidVar = "invalid_var"

// VarError is one problem with one variable.
type VarError struct {
	Var string
	Msg string
}

func (e *VarError) Error() string {
	if e.Var == "" {
		return fmt.Sprintf("%s (%s)", e.Msg, CodeInvalidVar)
	}
	return fmt.Sprintf("var %q: %s (%s)", e.Var, e.Msg, CodeInvalidVar)
}

// Declared var types (flow.md §3). An absent type is string.
const (
	VarString   = "string"
	VarInteger  = "integer"
	VarNumber   = "number"
	VarBoolean  = "boolean"
	VarDuration = "duration"
	VarList     = "list"
	VarMap      = "map"
)

// CoerceVar converts a `--var key=value` string to the var's declared type
// (flow.md §3): integer to int64, number to float64, boolean to bool,
// duration to a validated duration string, list and map from a JSON
// literal to []any and map[string]any, string and untyped as written.
// Failing coercion is a VarError (invalid_var).
func CoerceVar(name string, def spec.VarDef, raw string) (any, error) {
	bad := func(format string, args ...any) (any, error) {
		return nil, &VarError{Var: name, Msg: fmt.Sprintf(format, args...)}
	}
	switch def.Type {
	case "", VarString:
		return raw, nil
	case VarInteger:
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return bad("%q is not an integer", raw)
		}
		return n, nil
	case VarNumber:
		f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return bad("%q is not a number", raw)
		}
		return f, nil
	case VarBoolean:
		b, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return bad("%q is not a boolean (true or false)", raw)
		}
		return b, nil
	case VarDuration:
		if _, err := spec.ParseDuration(raw); err != nil {
			return bad("%q is not a duration (30s, 5m, 48h, 5d, 2w)", raw)
		}
		return strings.TrimSpace(raw), nil
	case VarList:
		var l []any
		if err := json.Unmarshal([]byte(raw), &l); err != nil || l == nil {
			return bad("%q is not a JSON list", raw)
		}
		return l, nil
	case VarMap:
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil || m == nil {
			return bad("%q is not a JSON object", raw)
		}
		return m, nil
	}
	return bad("unknown type %q", def.Type)
}

// ResolveVars is spec.ResolveVars with typing: provided strings are
// coerced to each var's declared type, defaults keep the type they were
// written with (a default whose type contradicts the declaration is V05).
// List and map values come from a JSON literal, which is also what a
// --var-file value is. Every failure is a VarError.
func ResolveVars(f *spec.Flow, provided map[string]string) (map[string]any, error) {
	resolved, err := spec.ResolveVars(f, provided)
	if err != nil {
		return nil, &VarError{Msg: err.Error()}
	}
	names := make([]string, 0, len(provided))
	for k := range provided {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		v, err := CoerceVar(k, f.Vars[k], provided[k])
		if err != nil {
			return nil, err
		}
		resolved[k] = v
	}
	return resolved, nil
}

// CheckVarType reports whether v is acceptable for a var declared with
// type typ: it is what a default must be (V05). Numbers decoded from YAML
// or JSON are accepted for integer when whole.
func CheckVarType(typ string, v any) error {
	ok := true
	switch typ {
	case "":
		// an untyped var takes any default
	case VarString:
		_, ok = v.(string)
	case VarInteger:
		n, isNum := asFloat(v)
		ok = isNum && n == math.Trunc(n)
	case VarNumber:
		_, ok = asFloat(v)
	case VarBoolean:
		_, ok = v.(bool)
	case VarDuration:
		s, isStr := v.(string)
		if ok = isStr; ok {
			_, err := spec.ParseDuration(s)
			ok = err == nil
		}
	case VarList:
		_, ok = v.([]any)
	case VarMap:
		_, ok = v.(map[string]any)
	}
	if !ok {
		return fmt.Errorf("%v (%T) is not a %s", v, v, typ)
	}
	return nil
}

func asFloat(v any) (float64, bool) {
	if v == nil {
		return 0, false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	}
	return 0, false
}
