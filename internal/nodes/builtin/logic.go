package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/nodes/render"
	"hop.top/flo/spec/node"
)

type fn struct {
	spec *node.Spec
	run  func(ctx context.Context, inv core.Invocation) (core.Result, error)
}

func (f fn) Spec() *node.Spec { return f.spec }
func (f fn) Execute(ctx context.Context, inv core.Invocation) (core.Result, error) {
	return f.run(ctx, inv)
}

func newDedupe() (core.BuiltinNode, error) {
	s, err := Spec("flo/dedupe@1")
	if err != nil {
		return nil, err
	}
	return fn{s, func(_ context.Context, inv core.Invocation) (core.Result, error) { return dedupe(inv), nil }}, nil
}

func newFilter() (core.BuiltinNode, error) {
	s, err := Spec("flo/filter@1")
	if err != nil {
		return nil, err
	}
	return fn{s, func(_ context.Context, inv core.Invocation) (core.Result, error) { return filter(inv), nil }}, nil
}

// pureFail is the failure of a pure node: nothing happened.
func pureFail(e *core.Error) core.Result {
	return core.Result{Outcome: core.OutcomeFailed, Err: e, Effect: core.EffectNone}
}

func pureOK(out any) core.Result {
	return core.Result{Outcome: core.OutcomeOK, Output: out, Effect: core.EffectDone}
}

// dedupe: node.md §6.1. The node is pure; the flow commits `seen`.
func dedupe(inv core.Invocation) core.Result {
	items, ok := list(inv.In["items"])
	if !ok {
		return pureFail(core.Errorf(core.CodeInputInvalid, "items is not a list"))
	}
	seen, _ := list(inv.In["seen"])
	max := asInt(inv.In["max"], 10000)
	src, _ := inv.In["key"].(string)
	env, err := render.Shared()
	if err != nil {
		return pureFail(core.Wrap(core.CodeInternal, err))
	}
	keyOf, cerr := env.Item(src)
	if cerr != nil {
		return pureFail(cerr)
	}

	known := map[string]bool{}
	var seenKeys []string
	for _, s := range seen {
		k := keyText(s)
		if !known[k] {
			known[k] = true
			seenKeys = append(seenKeys, k)
		}
	}
	out := []any{}
	keys := []any{}
	for i, it := range items {
		kv, kerr := keyOf(it, i)
		if kerr != nil {
			return pureFail(core.Wrap(core.CodeEvalError, kerr))
		}
		k := keyText(kv)
		if known[k] {
			continue // already seen, or an earlier item had it: first wins
		}
		known[k] = true
		out = append(out, it)
		keys = append(keys, k)
		seenKeys = append(seenKeys, k)
	}
	if len(seenKeys) > max {
		seenKeys = seenKeys[len(seenKeys)-max:]
	}
	seenOut := make([]any, len(seenKeys))
	for i, k := range seenKeys {
		seenOut[i] = k
	}
	return pureOK(map[string]any{"items": out, "keys": keys, "seen": seenOut})
}

// filter: node.md §6.2.
func filter(inv core.Invocation) core.Result {
	items, ok := list(inv.In["items"])
	if !ok {
		return pureFail(core.Errorf(core.CodeInputInvalid, "items is not a list"))
	}
	src, _ := inv.In["where"].(string)
	env, err := render.Shared()
	if err != nil {
		return pureFail(core.Wrap(core.CodeInternal, err))
	}
	where, cerr := env.Item(src)
	if cerr != nil {
		return pureFail(cerr)
	}
	out := []any{}
	for i, it := range items {
		v, werr := where(it, i)
		if werr != nil {
			return pureFail(core.Wrap(core.CodeEvalError, werr))
		}
		b, isBool := v.(bool)
		if !isBool {
			return pureFail(core.Errorf(core.CodeEvalError, "where must be a boolean, item %d gave %T", i, v))
		}
		if b {
			out = append(out, it)
		}
	}
	return pureOK(map[string]any{"items": out})
}

// keyText is the text of a key: a string as itself, anything else as JSON.
func keyText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// list reads a list however the caller built it ([]any from a template,
// []string or []map from Go).
func list(v any) ([]any, bool) {
	switch t := v.(type) {
	case nil:
		return nil, true
	case []any:
		return t, true
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice {
		return nil, false
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}

func asInt(v any, def int) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n)
		}
	}
	return def
}
