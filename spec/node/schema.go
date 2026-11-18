package node

import (
	"fmt"
	"math"
	"net/url"
	"reflect"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"hop.top/flo/spec"
)

// schemaTypes are the JSON-Schema type names of node.md §3.1.
var schemaTypes = map[string]bool{
	"string": true, "integer": true, "number": true, "boolean": true,
	"array": true, "object": true, "null": true,
}

// schemaOf renders an InputDef as the generic schema map CheckValue reads.
// The `required` flag of an input is a property of the input, not of the
// value, and is dropped; a list of names (object schemas) is kept.
func schemaOf(d InputDef) map[string]any {
	b, err := yaml.Marshal(d)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil || m == nil {
		return map[string]any{}
	}
	if _, isFlag := m["required"].(bool); isFlag {
		delete(m, "required")
	}
	// The generic form of nested defs carries `required: true` too.
	stripFlags(m)
	return m
}

func stripFlags(m map[string]any) {
	if props, ok := m["properties"].(map[string]any); ok {
		for _, p := range props {
			if pm, ok := p.(map[string]any); ok {
				if _, isFlag := pm["required"].(bool); isFlag {
					delete(pm, "required")
				}
				stripFlags(pm)
			}
		}
	}
	if it, ok := m["items"].(map[string]any); ok {
		if _, isFlag := it["required"].(bool); isFlag {
			delete(it, "required")
		}
		stripFlags(it)
	}
}

// CheckValue validates v against a JSON-Schema subset given as a generic
// map: type (one or a list), enum, minimum, maximum, items, properties,
// required (a list of names) and format (duration, date-time, uri). Keys it
// does not know are ignored. It returns one message per violation, each
// prefixed with the path of the offending value, in a stable order.
func CheckValue(schema map[string]any, v any) []string {
	var out []string
	checkSchema(schema, v, "", &out)
	return out
}

func at(path string) string {
	if path == "" {
		return "value"
	}
	return path
}

func checkSchema(schema map[string]any, v any, path string, out *[]string) {
	add := func(format string, args ...any) {
		*out = append(*out, at(path)+": "+fmt.Sprintf(format, args...))
	}
	if types := typeNames(schema["type"]); len(types) > 0 {
		ok := false
		for _, t := range types {
			if isType(t, v) {
				ok = true
				break
			}
		}
		if !ok {
			add("want %s, got %s", joinOr(types), typeOf(v))
			return
		}
	}
	if enum, ok := schema["enum"].([]any); ok && !inEnum(enum, v) {
		add("%v is not one of the allowed values", v)
	}
	checkBounds(schema, v, add)
	if f, ok := schema["format"].(string); ok {
		if s, isStr := v.(string); isStr {
			if msg := checkFormat(f, s); msg != "" {
				add("%s", msg)
			}
		}
	}
	checkComposite(schema, v, path, out)
}

func checkBounds(schema map[string]any, v any, add func(string, ...any)) {
	n, isNum := toFloat(v)
	if !isNum {
		return
	}
	if lo, ok := toFloat(schema["minimum"]); ok && n < lo {
		add("%v is below the minimum %v", v, schema["minimum"])
	}
	if hi, ok := toFloat(schema["maximum"]); ok && n > hi {
		add("%v is above the maximum %v", v, schema["maximum"])
	}
}

func checkComposite(schema map[string]any, v any, path string, out *[]string) {
	switch t := v.(type) {
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, e := range t {
				checkSchema(items, e, fmt.Sprintf("%s[%d]", path, i), out)
			}
		}
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if name, ok := r.(string); ok {
					if _, present := t[name]; !present {
						*out = append(*out, fmt.Sprintf("%s: required property %q is missing", at(path), name))
					}
				}
			}
		}
		for _, k := range keys {
			pm, ok := props[k].(map[string]any)
			if !ok {
				continue
			}
			if val, present := t[k]; present {
				checkSchema(pm, val, join(path, k), out)
			}
		}
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func typeNames(t any) []string {
	switch x := t.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return x
	}
	return nil
}

func joinOr(ts []string) string {
	s := ""
	for i, t := range ts {
		if i > 0 {
			s += " or "
		}
		s += t
	}
	return s
}

func isType(t string, v any) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "integer":
		n, ok := toFloat(v)
		return ok && n == math.Trunc(n)
	case "number":
		_, ok := toFloat(v)
		return ok
	}
	return false
}

func typeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	if _, ok := toFloat(v); ok {
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func toFloat(v any) (float64, bool) {
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

func inEnum(enum []any, v any) bool {
	for _, e := range enum {
		if reflect.DeepEqual(e, v) {
			return true
		}
		a, aok := toFloat(e)
		b, bok := toFloat(v)
		if aok && bok && a == b {
			return true
		}
	}
	return false
}

func checkFormat(format, s string) string {
	switch format {
	case "duration":
		if _, err := spec.ParseDuration(s); err != nil {
			return fmt.Sprintf("%q is not a duration", s)
		}
	case "date-time":
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			return fmt.Sprintf("%q is not an RFC 3339 date-time", s)
		}
	case "uri":
		if u, err := url.Parse(s); err != nil || u.Scheme == "" {
			return fmt.Sprintf("%q is not a URI", s)
		}
	}
	return ""
}
