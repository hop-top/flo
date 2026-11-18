package cmd

import (
	"context"

	"hop.top/flo/internal/core"
)

// ServicesFunc opens the ports a command works against. Commands call it
// when they run, not when the tree is built, so building the tree (help,
// conformance, spec) touches no database.
type ServicesFunc func(ctx context.Context) (*core.Services, error)

// defaultServices is what NewRoot wires. Engine assembly replaces it with the
// real stores; until then a command that needs services fails clearly. Tests
// of a command call NewRootWith with coretest fakes.
var defaultServices ServicesFunc = func(context.Context) (*core.Services, error) {
	return nil, core.New(core.CodeEngineUnreachable, "the engine services are not wired in this build")
}
