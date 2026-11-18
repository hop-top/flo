// Package cmd wires the flo command tree on hop.top/kit.
package cmd

import (
	"context"

	kitcli "hop.top/kit/go/console/cli"

	"hop.top/flo/internal/version"
)

// NewRoot builds the flo root command. Kept separate from Execute so
// the conformance test can assert on the tree without running it.
func NewRoot() *kitcli.Root { return NewRootWith(defaultServices) }

// NewRootWith builds the tree over the given services, which is how tests of
// a command run it against fakes.
func NewRootWith(svc ServicesFunc) *kitcli.Root {
	root := kitcli.New(
		kitcli.Config{
			Name:    "flo",
			Version: version.Version(),
			Short:   "flo",
		},
		// Reserved `status` subcommand (12fc factor 8).
		kitcli.WithStatus(kitcli.StatusConfig{
			ExtraEnvKeys: []string{"FLO_*"},
		}),
	)
	addRun(root, svc)
	addApproval(root, svc)
	addGrant(root, svc)
	addTrigger(root, svc)
	addEngine(root, svc)
	addFlow(root, svc)
	return root
}

// Execute runs the flo CLI.
func Execute() error {
	return NewRoot().Execute(context.Background())
}
