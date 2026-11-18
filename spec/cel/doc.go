// Package cel compiles and evaluates the CEL expressions of a flow
// (flow spec §4): conditions (`when`, `until`) and `{{ }}` placeholders.
//
// It is a separate package so that importers of hop.top/flo/spec that only
// parse and expand flows do not link the CEL library. It adds, on top of
// spec's validator, compile-time checking of every expression (Validate,
// ParseFlow) and a pure evaluation API for an engine: Condition.Eval,
// Template.Render and Expr.Eval take an activation map and return typed
// values; nothing here performs I/O.
//
// # Environment
//
// Namespaces of flow spec §4.2 are declared as variables: vars, subject,
// run, steps, results, trigger, state, config, in, result and decisions are
// maps of dyn; item is dyn; index is int. Flow vars are also declared by
// bare name (§4.1) with their declared type: string, integer, boolean,
// list and map are typed; number and duration are dyn. The lists and
// strings extensions are enabled.
//
// # Limits of static checking
//
// Only top-level types are known. Fields read through results, state,
// trigger and the other maps are dyn, so a misuse of a field's type
// (results.a.count + 'x') compiles and fails at evaluation. Node specs are
// not consulted: with values are not checked against node inputs.
package cel
