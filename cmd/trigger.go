package cmd

import (
	"github.com/spf13/cobra"

	kitcli "hop.top/kit/go/console/cli"
)

// addTrigger registers the `flo trigger` noun. Its verbs are added by the piece of
// work that owns them; svc opens the ports they run against.
func addTrigger(root *kitcli.Root, svc ServicesFunc) {
	_ = svc
	root.Cmd.AddCommand(&cobra.Command{
		Use:   "trigger",
		Short: "trigger commands",
	})
}
