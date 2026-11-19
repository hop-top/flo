// Package cmd wires the flo command tree on hop.top/kit.
package cmd

import (
	"context"
	"errors"

	kitcli "hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output/envelope"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/version"
)

// NewRoot builds the flo root command over the real engine: the stores under
// the data root, opened when a command runs. Kept separate from Execute so
// the conformance test can assert on the tree without running it.
func NewRoot() *kitcli.Root {
	var root *kitcli.Root
	o := engineOf(func() *kitcli.Root { return root })
	root = build(o.services, o.engine, true)
	return root
}

// NewRootWith builds the tree over the given services, which is how tests of
// a command run it against fakes. Without an engine, `flo engine serve` and
// the flow commands fail with engine_unreachable.
func NewRootWith(svc ServicesFunc) *kitcli.Root { return build(svc, nil, false) }

// NewRootOver builds the tree over an engine opened by open: the real
// commands against a real, possibly differently configured, engine (tests
// that run the whole assembly in process).
func NewRootOver(open EngineFunc) *kitcli.Root {
	o := &opener{open: open}
	return build(o.services, o.engine, true)
}

func build(svc ServicesFunc, eng EngineFunc, real bool) *kitcli.Root {
	var root *kitcli.Root
	cfg := &configLoader{}
	defer func() { loaders.Store(root, cfg) }()
	root = kitcli.New(
		kitcli.Config{
			Name:    "flo",
			Version: version.Version(),
			Short:   "flo",
			// -c files, the config files and FLO_PACKS_* load before a
			// command runs.
			Hooks: kitcli.Hooks{PrePersistentRunE: cfg.hook(func() *kitcli.Root { return root })},
		},
		// Reserved `status` subcommand (12fc factor 8).
		kitcli.WithStatus(kitcli.StatusConfig{
			ExtraEnvKeys: []string{"FLO_*"},
		}),
	)
	bindEnv(root)
	cfg.registerStatus(root)
	var alive func() error
	if real {
		alive = engineAlive
		registerEngineStatus(root)
	}
	addRun(root, svc, eng, alive)
	addApproval(root, svc)
	addBrief(root, svc)
	addGrant(root, svc)
	addAccess(root, svc)
	addCapabilities(root)
	addTrigger(root, svc)
	addEngine(root, eng)
	addFlow(root, svc, eng)
	return root
}

// Execute runs the flo CLI.
func Execute() error {
	return NewRoot().Execute(context.Background())
}

// ExitCode is the process exit code of err (the table of core/errors.go): the
// code of a flo error, the exit code a kit error carries (a usage error is
// 2), else 1.
func ExitCode(err error) int {
	if err == nil {
		return core.ExitOK
	}
	var fe *core.Error
	if errors.As(err, &fe) {
		return fe.Exit()
	}
	var ce interface{ AsCLIError() *envelope.Error }
	if errors.As(err, &ce) {
		if out := ce.AsCLIError(); out != nil && out.ExitCode != 0 {
			return out.ExitCode
		}
	}
	return core.ExitGeneral
}
