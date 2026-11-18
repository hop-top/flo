package flowtest

import (
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"

	"hop.top/flo/internal/core"
	specrun "hop.top/flo/spec/run"
)

// Where a test's flow state came from.
const (
	StateDefault  = "default"  // the declared defaults
	StateCurrent  = "current"  // a read-only copy of the engine's committed state
	StateRecorded = "recorded" // record/state.json
	StateFile     = "file:"    // prefix: a seed file named with --state
	StateCase     = "case:"    // prefix: the state: map of a tests/ document
)

// ReadSeedFile reads a seed: a JSON or YAML mapping of state key to value.
func ReadSeedFile(path string) (map[string]any, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the file the caller named
	if err != nil {
		return nil, core.Errorf(core.CodeUsage, "--state %s: %v", path, err)
	}
	return DecodeSeed(b, "--state "+path)
}

// DecodeSeed decodes a seed document (JSON is YAML).
func DecodeSeed(b []byte, what string) (map[string]any, error) {
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, core.Errorf(core.CodeUsage, "%s: want a JSON or YAML mapping: %v", what, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	out, err := core.NormalizeJSON(m)
	if err != nil {
		return nil, core.Errorf(core.CodeUsage, "%s: %v", what, err)
	}
	return out.(map[string]any), nil //nolint:forcetypeassert // NormalizeJSON keeps a map a map
}

// kindOf names the JSON kind of a value.
func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "list"
	case map[string]any:
		return "map"
	}
	return "number"
}

// ValidateSeed checks a seed against the flow's declared state: every key is
// declared (a state key is declared as { default: v }), and a value has the
// JSON kind of the key's default (a null default accepts any kind, and a null
// value is always allowed). what names the seed in messages.
func ValidateSeed(decls map[string]specrun.StateDecl, seed map[string]any, what string) error {
	keys := make([]string, 0, len(seed))
	for k := range seed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d, ok := decls[k]
		if !ok {
			return core.Errorf(core.CodeUsage, "%s: %q is not a state key of the flow", what, k).
				WithSuggestion("declared keys: " + declared(decls))
		}
		want, got := kindOf(normOrSelf(d.Default)), kindOf(seed[k])
		if want != "null" && got != "null" && want != got {
			return core.Errorf(core.CodeUsage, "%s: state key %q is declared as a %s, got a %s", what, k, want, got)
		}
	}
	return nil
}

func normOrSelf(v any) any {
	if n, err := core.NormalizeJSON(v); err == nil {
		return n
	}
	return v
}

func declared(decls map[string]specrun.StateDecl) string {
	keys := make([]string, 0, len(decls))
	for k := range decls {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "none"
	}
	return fmt.Sprint(keys)
}
