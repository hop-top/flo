package cel

import (
	"fmt"
	"sort"

	celgo "cel.dev/cel-go/cel"
	"cel.dev/cel-go/ext"

	"hop.top/flo/spec"
)

// Activation is the evaluation input: namespace name to value. Engines bind
// the namespaces of flow spec §4.2 (vars, results, trigger, state, item,
// index, ...); flow vars are also read by bare name, see BindVars.
type Activation = map[string]any

// namespaces declared as map<string, dyn>.
var mapNamespaces = []string{
	"vars", "subject", "run", "steps", "results", "trigger", "state", "config", "in", "result", "decisions",
}

// celReserved are words CEL cannot use as a variable name; a flow var with
// one of these names is reachable only as vars.<name>.
var celReserved = map[string]bool{
	"as": true, "break": true, "const": true, "continue": true, "else": true, "false": true,
	"for": true, "function": true, "if": true, "import": true, "in": true, "let": true,
	"loop": true, "package": true, "namespace": true, "null": true, "return": true,
	"true": true, "var": true, "void": true, "while": true,
}

// Env compiles expressions against the flow-spec namespaces. It is safe
// for concurrent use.
type Env struct {
	env   *celgo.Env
	known map[string]bool // step ids, for hyphenated results.<id> references
}

// NewEnv returns an environment with the namespaces of flow spec §4.2 and
// no flow vars.
func NewEnv() (*Env, error) { return newEnv(nil, nil) }

// EnvForFlow returns an environment that also declares the flow's vars by
// bare name (typed where the flow declares a type) and knows the flow's
// step ids, so results.<id> accepts ids with "-".
func EnvForFlow(f *spec.Flow) (*Env, error) {
	return newEnv(f.Vars, collectStepIDs(f.Steps, nil))
}

func newEnv(vars map[string]spec.VarDef, ids []string) (*Env, error) {
	dyn := celgo.MapType(celgo.StringType, celgo.DynType)
	opts := []celgo.EnvOption{ext.Lists(), ext.Strings(),
		celgo.Variable("item", celgo.DynType), celgo.Variable("index", celgo.IntType)}
	for _, ns := range mapNamespaces {
		opts = append(opts, celgo.Variable(ns, dyn))
	}
	names := make([]string, 0, len(vars))
	for n := range vars {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if celReserved[n] || isNamespace(n) {
			continue
		}
		opts = append(opts, celgo.Variable(n, varType(vars[n])))
	}
	env, err := celgo.NewEnv(opts...)
	if err != nil {
		return nil, fmt.Errorf("cel env: %w", err)
	}
	known := make(map[string]bool, len(ids))
	for _, id := range ids {
		known[id] = true
	}
	return &Env{env: env, known: known}, nil
}

func newTriggerEnv() (*Env, error) {
	env, err := celgo.NewEnv(ext.Lists(), ext.Strings(),
		celgo.Variable("trigger", celgo.MapType(celgo.StringType, celgo.DynType)))
	if err != nil {
		return nil, fmt.Errorf("cel env: %w", err)
	}
	return &Env{env: env, known: map[string]bool{}}, nil
}

func isNamespace(n string) bool {
	if n == "item" || n == "index" {
		return true
	}
	for _, ns := range mapNamespaces {
		if ns == n {
			return true
		}
	}
	return false
}

// varType maps a declared var type to a CEL type. number and duration, and
// an untyped var whose default is not a string, are dyn: YAML numbers may
// be int or double, and durations are strings an engine may coerce.
func varType(d spec.VarDef) *celgo.Type {
	switch d.Type {
	case "string":
		return celgo.StringType
	case "integer":
		return celgo.IntType
	case "boolean":
		return celgo.BoolType
	case "list":
		return celgo.ListType(celgo.DynType)
	case "map":
		return celgo.MapType(celgo.StringType, celgo.DynType)
	case "":
		if _, isString := d.Default.(string); isString || d.Default == nil {
			return celgo.StringType
		}
	}
	return celgo.DynType
}

// BindVars sets the vars namespace of act and every var by bare name
// (flow spec §4.1), returning act. Bare names that collide with a
// namespace or a CEL keyword are skipped.
func BindVars(act Activation, vars map[string]any) Activation {
	act["vars"] = vars
	for n, v := range vars {
		if !celReserved[n] && !isNamespace(n) {
			act[n] = v
		}
	}
	return act
}

func collectStepIDs(steps []spec.Step, out []string) []string {
	for i := range steps {
		out = append(out, steps[i].ID)
		out = collectStepIDs(steps[i].Steps, out)
	}
	return out
}
