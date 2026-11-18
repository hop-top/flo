// Package spec is the plan level of the flo flow specification: parse,
// validate, expand include and repeat, render, select, and resolve flow
// references. It imports no engine and no task store; materializing steps
// into tasks and running them belong to the implementer that uses it.
//
// The grammar follows docs/spec/flow.md. Run-level keys (on, state,
// run_as, privacy, budget, tasks, concurrency, returns, for_each, uses,
// outputs, ...) are parsed and kept but not interpreted. Conditions are
// evaluated only in the single-comparison form of recipe spec 0.1; a CEL
// expression is accepted unparsed and reported as ErrNeedsCEL on
// evaluation. Compiling and evaluating CEL is the subpackage
// hop.top/flo/spec/cel, kept apart so this package links no CEL library.
package spec
