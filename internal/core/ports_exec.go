package core

import (
	"context"

	"hop.top/flo/spec/node"
)

// Runner places an invocation and runs it: it checks the placement the node
// asks for (runner class, isolation, privacy), refuses what phase 1 cannot
// do (a runner other than local, isolation beyond process), and hands the
// invocation to a Dispatcher. The executor calls the Runner, never the
// Dispatcher (D2: steps run in-process behind the Runner seam, so a remote
// runner can replace it later).
type Runner interface {
	// Run executes one attempt. A step failure is a Result with
	// OutcomeFailed; the error return is for faults of the engine itself
	// (the executor records it as a failed attempt of unknown effect).
	Run(ctx context.Context, inv Invocation) (Result, error)
}

// Dispatcher routes an invocation by its kind: a core exec or human step, a
// declared command or HTTP node, a built-in, a plugin. It validates the
// inputs against the node spec before the call and the output after
// (output_invalid), enforces dry-run (a write-shared or irreversible node
// returns its Plan and does not execute), draws on rate-limit buckets, and
// turns a call into a Result. Same error contract as Runner.
type Dispatcher interface {
	Dispatch(ctx context.Context, inv Invocation) (Result, error)
}

// NodeRegistry resolves node references to specs, through the layers of
// node.md §1.2: the flow bundle's nodes/, installed packs, built-ins.
type NodeRegistry interface {
	// Resolve returns the spec for ref. A ref `ns/name@N` pins the major;
	// `ns/name` takes the highest N. node_not_found when no layer has it; a
	// spec declared by two packs is node_conflict.
	Resolve(ctx context.Context, scope NodeScope, ref string) (NodeInfo, error)
	// List returns every node visible from scope, one per ns/name@N, by type.
	List(ctx context.Context, scope NodeScope) ([]NodeInfo, error)
}

// BuiltinNode is a node implemented by the engine. Its spec is shipped with
// the engine (embedded) and follows node.md §1.
type BuiltinNode interface {
	// Spec returns the node's spec.
	Spec() *node.Spec
	// Execute runs one invocation with its inputs already validated.
	Execute(ctx context.Context, inv Invocation) (Result, error)
}

// BuiltinRegistry holds the built-in nodes (flo/dedupe@1, flo/filter@1,
// flo/approval@1, flo/output@1, ...). The NodeRegistry serves their specs;
// the Dispatcher executes through it.
type BuiltinRegistry interface {
	// Register adds a built-in under its spec's node type. A second
	// registration of the same type is node_conflict.
	Register(n BuiltinNode) error
	// Lookup returns the built-in for a node type `ns/name@N`.
	Lookup(ref string) (BuiltinNode, bool)
	// List returns the built-ins ordered by node type.
	List() []BuiltinNode
}

// CommandRunner runs a process: the one place the engine spawns anything.
// Declared command nodes, core exec steps, trust providers and the task
// adapter all go through it, so tests substitute one fake for every process.
type CommandRunner interface {
	// Run executes spec. An error is returned only when the process could
	// not be started (the effect is none); a non-zero exit, a timeout and a
	// truncated stream are reported in the CommandResult.
	Run(ctx context.Context, spec CommandSpec) (CommandResult, error)
}

// Checker sends a step's output to the contract gateway (eva) and reports
// its verdict (flow.md §5.8).
type Checker interface {
	// Check returns Passed for a 200 and a failing CheckResult for a 422.
	// Any other status, and an unreachable gateway, is a transient
	// check_unavailable error.
	Check(ctx context.Context, req CheckRequest) (CheckResult, error)
}

// Secrets resolves a secret by reference for a profile (node.md §5.4). The
// value is never rendered into a record, a log or a cassette.
type Secrets interface {
	// Get returns the secret's value, or secret_not_found.
	Get(ctx context.Context, tenant TenantID, profile, name string) ([]byte, error)
}
