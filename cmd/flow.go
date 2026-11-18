package cmd

import (
	"github.com/spf13/cobra"

	kitcli "hop.top/kit/go/console/cli"
)

// addFlow registers the `flo flow` noun. Its verbs are added by the piece of
// work that owns them; svc opens the ports they run against.
func addFlow(root *kitcli.Root, svc ServicesFunc) {
	_ = svc
	root.Cmd.AddCommand(&cobra.Command{
		Use:   "flow",
		Short: "flow commands",
	})
}
