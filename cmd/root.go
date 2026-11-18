// Package cmd wires the flo command tree on hop.top/kit.
package cmd

import (
	"context"

	kitcli "hop.top/kit/go/console/cli"

	"hop.top/flo/internal/version"
)

// NewRoot builds the flo root command. Kept separate from Execute so
// the conformance test can assert on the tree without running it.
func NewRoot() *kitcli.Root {
	return kitcli.New(
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
}

// Execute runs the flo CLI.
func Execute() error {
	return NewRoot().Execute(context.Background())
}
